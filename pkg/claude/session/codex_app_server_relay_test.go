package session

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteCodexAppServerClientMessagePreservesManagedToolProfile(t *testing.T) {
	const profile = "tclaude-agent-0123456789abcdef"
	tests := []struct {
		name       string
		input      string
		wantField  string
		wantAbsent string
		unchanged  bool
	}{
		{
			name:      "fresh remote thread",
			input:     `{"id":1,"method":"thread/start","params":{"cwd":"/repo","sandbox":"workspace-write","sandboxPolicy":{"type":"readOnly"}}}`,
			wantField: "permissions", wantAbsent: "sandbox",
		},
		{
			name:      "remote resume",
			input:     `{"id":2,"method":"thread/resume","params":{"threadId":"thread","sandbox":"read-only"}}`,
			wantField: "permissions", wantAbsent: "sandbox",
		},
		{
			name:      "turn legacy override",
			input:     `{"id":3,"method":"turn/start","params":{"threadId":"thread","input":[],"sandboxPolicy":{"type":"workspaceWrite"}}}`,
			wantField: "permissions", wantAbsent: "sandboxPolicy",
		},
		{
			name:      "turn conflicting named permissions are clamped",
			input:     `{"id":35,"method":"turn/start","params":{"threadId":"thread","input":[],"permissions":"other","sandboxPolicy":{"type":"readOnly"}}}`,
			wantField: "permissions", wantAbsent: "sandboxPolicy",
		},
		{
			name:      "named turn override is clamped",
			input:     `{"id":4,"method":"thread/settings/update","params":{"threadId":"thread","permissions":"other"}}`,
			wantField: "permissions",
		},
		{
			name:      "non-permission settings update inherits thread profile",
			input:     `{"id":45,"method":"thread/settings/update","params":{"threadId":"thread","model":"gpt-5"}}`,
			unchanged: true,
		},
		{
			name:      "typed agentd turn inherits corrected thread profile",
			input:     `{"id":5,"method":"turn/start","params":{"threadId":"thread","input":[{"type":"text","text":"hello"}]}}`,
			unchanged: true,
		},
		{
			name:      "unrelated call",
			input:     `{"id":6,"method":"thread/read","params":{"threadId":"thread"}}`,
			unchanged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteCodexAppServerClientMessage([]byte(tc.input), profile)
			require.NoError(t, err)
			if tc.unchanged {
				assert.Equal(t, tc.input, string(got))
				return
			}
			var request struct {
				Params map[string]json.RawMessage `json:"params"`
			}
			require.NoError(t, json.Unmarshal(got, &request))
			require.Contains(t, request.Params, tc.wantField)
			assert.JSONEq(t, `"`+profile+`"`, string(request.Params[tc.wantField]))
			if tc.wantAbsent != "" {
				assert.NotContains(t, request.Params, tc.wantAbsent)
			}
			assert.NotContains(t, request.Params, "sandboxPolicy")
		})
	}
}

func TestRewriteCodexAppServerClientMessageFailsClosedForRelevantMalformedParams(t *testing.T) {
	for _, input := range []string{
		`{"id":1,"method":"thread/start"}`,
		`{"id":2,"method":"thread/resume","params":null}`,
		`{"id":3,"method":"thread/settings/update","params":[]}`,
		`{"id":4,"method":"turn/start","params":"bad"}`,
	} {
		_, err := rewriteCodexAppServerClientMessage([]byte(input), "tclaude-agent-profile")
		assert.Error(t, err, input)
	}

	for _, input := range []string{
		`not-json`,
		`{"id":5,"method":"thread/read","params":[]}`,
		`{"id":6,"result":{"sandbox":"opaque"}}`,
	} {
		got, err := rewriteCodexAppServerClientMessage([]byte(input), "tclaude-agent-profile")
		require.NoError(t, err)
		assert.Equal(t, input, string(got))
	}
}

func TestCodexAppServerProfileRelayPreservesNativeAuthAndEnforcesEveryWriter(t *testing.T) {
	const (
		token   = "native-generation-capability"
		profile = "tclaude-agent-0123456789abcdef"
	)
	approval := []byte(`{"id":71,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread","reason":"owned by TUI"}}`)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamListener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	upstreamServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		defer conn.Close()
		if writeErr := conn.WriteMessage(websocket.TextMessage, approval); writeErr != nil {
			return
		}
		for {
			messageType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if writeErr := conn.WriteMessage(messageType, payload); writeErr != nil {
				return
			}
		}
	})}
	go func() { _ = upstreamServer.Serve(upstreamListener) }()
	t.Cleanup(func() { require.NoError(t, upstreamServer.Close()) })

	relayContext, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	relayDir, err := os.MkdirTemp("/tmp", "tclaude-relay-test-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(relayDir)) })
	socketPath := filepath.Join(relayDir, "app.sock")
	go func() {
		relayDone <- runCodexAppServerRelay(
			relayContext, socketPath, upstreamListener.Addr().String(), profile)
	}()
	t.Cleanup(func() {
		stopRelay()
		require.NoError(t, <-relayDone)
	})
	require.Eventually(t, func() bool {
		info, statErr := os.Lstat(socketPath)
		return statErr == nil && info.Mode()&os.ModeSocket != 0
	}, 5*time.Second, 10*time.Millisecond)

	dial := func(t *testing.T, bearer string) (*websocket.Conn, *http.Response, error) {
		t.Helper()
		dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}}
		headers := http.Header{}
		if bearer != "" {
			headers.Set("Authorization", "Bearer "+bearer)
		}
		conn, response, dialErr := dialer.Dial("ws://localhost/", headers)
		return conn, response, dialErr
	}

	for _, bearer := range []string{"", "wrong-capability"} {
		conn, response, dialErr := dial(t, bearer)
		require.Error(t, dialErr)
		assert.Nil(t, conn)
		require.NotNil(t, response)
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode,
			"the native upstream, not the relay, must decide authentication")
		require.NoError(t, response.Body.Close())
	}

	conn, response, err := dial(t, token)
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	require.NoError(t, response.Body.Close())
	t.Cleanup(func() { _ = conn.Close() })
	messageType, payload, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)
	assert.Equal(t, approval, payload, "server approval requests must remain TUI-owned and byte-identical")

	requests := []struct {
		input  string
		method string
	}{
		{`{"id":1,"method":"thread/start","params":{"cwd":"/repo","sandbox":"workspace-write"}}`, "thread/start"},
		{`{"id":2,"method":"thread/resume","params":{"threadId":"thread","sandbox":"read-only"}}`, "thread/resume"},
		{`{"id":3,"method":"thread/settings/update","params":{"threadId":"thread","model":"gpt-5","sandboxPolicy":{"type":"dangerFullAccess"}}}`, "thread/settings/update"},
		{`{"id":4,"method":"turn/start","params":{"threadId":"thread","input":[],"permissions":"host","sandbox":"danger-full-access","sandboxPolicy":{"type":"dangerFullAccess"}}}`, "turn/start"},
	}
	for _, request := range requests {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(request.input)))
		messageType, payload, err = conn.ReadMessage()
		require.NoError(t, err, request.method)
		assert.Equal(t, websocket.TextMessage, messageType)
		var got struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		require.NoError(t, json.Unmarshal(payload, &got))
		assert.Equal(t, request.method, got.Method)
		assert.JSONEq(t, `"`+profile+`"`, string(got.Params["permissions"]))
		assert.NotContains(t, got.Params, "sandbox")
		assert.NotContains(t, got.Params, "sandboxPolicy")
	}

	unrelated := []byte(`{"id":81,"result":{"decision":"acceptForSession","sandboxPolicy":{"opaque":true}}}`)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, unrelated))
	_, payload, err = conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, unrelated, payload, "approval responses and unrelated messages must remain byte-identical")
	binary := []byte{0, 1, 2, 3, 255}
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, binary))
	messageType, payload, err = conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, messageType)
	assert.Equal(t, binary, payload)

	malformed, response, err := dial(t, token)
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	require.NoError(t, response.Body.Close())
	defer malformed.Close()
	_, _, err = malformed.ReadMessage() // approval request
	require.NoError(t, err)
	require.NoError(t, malformed.WriteMessage(websocket.TextMessage,
		[]byte(`{"id":91,"method":"turn/start","params":null}`)))
	_ = malformed.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = malformed.ReadMessage()
	assert.Error(t, err, "a relevant malformed request must be dropped with its connection")
}

func TestConsumeCodexAppServerTokenIsOneShotAndRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capability")
	require.NoError(t, os.WriteFile(path, []byte("launch-secret\n"), 0o600))

	token, err := consumeCodexAppServerToken(path)
	require.NoError(t, err)
	assert.Equal(t, "launch-secret", token)
	_, err = consumeCodexAppServerToken(path)
	assert.Error(t, err, "the handoff must be consumed exactly once")

	unsafe := filepath.Join(dir, "unsafe")
	require.NoError(t, os.WriteFile(unsafe, []byte("secret"), 0o644))
	_, err = consumeCodexAppServerToken(unsafe)
	assert.ErrorContains(t, err, "owned private file")

	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	symlink := filepath.Join(dir, "symlink")
	require.NoError(t, os.Symlink(target, symlink))
	_, err = consumeCodexAppServerToken(symlink)
	assert.ErrorContains(t, err, "owned private file")
}
