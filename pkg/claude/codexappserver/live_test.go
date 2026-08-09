package codexappserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Run with TCLAUDE_CODEX_APPSERVER_LIVE=1. The normal suite requires neither
// an installed Codex binary nor authentication.
func TestLiveCodexAppServerHandshake(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_APPSERVER_LIVE") != "1" {
		t.Skip("set TCLAUDE_CODEX_APPSERVER_LIVE=1 to run against installed Codex")
	}
	versionOutput, err := exec.Command("codex", "--version").Output()
	require.NoError(t, err)
	version := strings.TrimSpace(strings.TrimPrefix(string(versionOutput), "codex-cli "))
	require.NoError(t, codexappserver.CheckVersion(version))

	runtimeDir, err := os.MkdirTemp("/tmp", "codexappserver-live-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(runtimeDir)) })
	codexHome := filepath.Join(runtimeDir, "home")
	require.NoError(t, os.Mkdir(codexHome, 0o700))
	socketPath := filepath.Join(runtimeDir, "app.sock")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := listener.Addr().String()
	require.NoError(t, listener.Close())
	const token = "real-codex-generation-capability"
	digest := sha256.Sum256([]byte(token))

	processCtx, stopProcess := context.WithCancel(context.Background())
	var output bytes.Buffer
	command := exec.CommandContext(processCtx, "codex",
		"-c", `sandbox_mode="read-only"`,
		"-c", `approval_policy="never"`,
		"-c", `bypass_hook_trust=true`,
		"-c", `shell_environment_policy.set.TCLAUDE_APPSERVER_PROBE="present"`,
		"app-server",
		"--listen", "ws://"+upstream,
		"--ws-auth", "capability-token",
		"--ws-token-sha256", hex.EncodeToString(digest[:]))
	command.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	command.Stdout = &output
	command.Stderr = &output
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		stopProcess()
		_ = command.Wait()
	})
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	go func() { relayDone <- session.RunCodexAppServerRelay(relayCtx, socketPath, upstream) }()
	t.Cleanup(func() {
		stopRelay()
		require.NoError(t, <-relayDone)
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, statErr := os.Lstat(socketPath); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Codex app-server socket did not appear: %s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	for {
		probe, dialErr := net.DialTimeout("tcp", upstream, 100*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("authenticated Codex app-server did not listen: %s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	unauthorized, unauthorizedErr := codexappserver.Dial(ctx, socketPath,
		&codexappserver.Options{CodexVersion: version})
	require.Error(t, unauthorizedErr, "known relay endpoint must not authorize a client")
	require.Nil(t, unauthorized)
	client, err := codexappserver.Dial(ctx, socketPath, &codexappserver.Options{
		CodexVersion: version, BearerToken: token,
	})
	require.NoError(t, err, output.String())
	defer client.Close()
	_, err = client.ListLoadedThreads(ctx, codexappserver.ThreadLoadedListParams{})
	require.NoError(t, err)
	var effective struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, client.Call(ctx, "config/read", map[string]any{
		"cwd": runtimeDir, "includeLayers": false,
	}, &effective))
	require.Equal(t, "read-only", effective.Config["sandbox_mode"],
		"the app-server must apply the sandbox posture through its config seam")
	require.Equal(t, "never", effective.Config["approval_policy"],
		"the app-server must apply the approval posture through its config seam")
	// bypass_hook_trust is a launch extension rather than a persisted config
	// field, so config/read intentionally does not project it. Successful server
	// startup still exercises 0.147's typed boolean override parser; Codex emits
	// a hard error for a missing/non-boolean value.
	shellPolicy, ok := effective.Config["shell_environment_policy"].(map[string]any)
	require.True(t, ok, "config/read omitted shell_environment_policy: %#v", effective.Config)
	set, ok := shellPolicy["set"].(map[string]any)
	require.True(t, ok, "config/read omitted shell_environment_policy.set: %#v", shellPolicy)
	require.Equal(t, "present", set["TCLAUDE_APPSERVER_PROBE"])
}

func TestLiveCodexAppServerManagedPermissionOverlay(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_APPSERVER_LIVE") != "1" {
		t.Skip("set TCLAUDE_CODEX_APPSERVER_LIVE=1 to run against installed Codex")
	}
	versionOutput, err := exec.Command("codex", "--version").Output()
	require.NoError(t, err)
	version := strings.TrimSpace(strings.TrimPrefix(string(versionOutput), "codex-cli "))
	require.NoError(t, codexappserver.CheckVersion(version))

	runtimeDir, err := os.MkdirTemp("/tmp", "codexappserver-profile-live-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(runtimeDir)) })
	codexHome := filepath.Join(runtimeDir, "home")
	require.NoError(t, os.Mkdir(codexHome, 0o700))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := listener.Addr().String()
	require.NoError(t, listener.Close())
	readPath := filepath.Join(runtimeDir, "read")
	writePath := filepath.Join(runtimeDir, "write")
	denyPath := filepath.Join(runtimeDir, "deny")
	agentdSocket := filepath.Join(runtimeDir, "agentd.sock")
	relaySocket := filepath.Join(runtimeDir, "app.sock")
	relayReservation, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	relayURL := "ws://" + relayReservation.Addr().String()
	require.NoError(t, relayReservation.Close())
	for _, path := range []string{readPath, writePath, denyPath} {
		require.NoError(t, os.Mkdir(path, 0o700))
	}
	const profileName = "tclaude-live-profile"
	permissionTable := `permissions.` + profileName + `={extends=":workspace",filesystem={` +
		strconv.Quote(readPath) + `="read",` + strconv.Quote(writePath) + `="write",` +
		strconv.Quote(denyPath) + `="none",` + strconv.Quote(agentdSocket) + `="read"},` +
		`network={enabled=true,unix_sockets={` + strconv.Quote(agentdSocket) + `="allow"}}}`
	const token = "real-codex-profile-relay-capability"
	tokenDigest := sha256.Sum256([]byte(token))

	processCtx, stopProcess := context.WithCancel(context.Background())
	var output bytes.Buffer
	command := exec.CommandContext(processCtx, "codex",
		"-c", `default_permissions="`+profileName+`"`,
		"-c", permissionTable,
		"app-server", "--listen", "ws://"+upstream,
		"--ws-auth", "capability-token",
		"--ws-token-sha256", hex.EncodeToString(tokenDigest[:]))
	command.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	command.Stdout = &output
	command.Stderr = &output
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		stopProcess()
		_ = command.Wait()
	})
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- session.RunCodexAppServerProfileRelay(
			relayCtx, relaySocket, relayURL, upstream, profileName)
	}()
	t.Cleanup(func() {
		stopRelay()
		require.NoError(t, <-relayDone)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var client *codexappserver.Client
	for ctx.Err() == nil {
		client, err = codexappserver.Dial(ctx, relaySocket, &codexappserver.Options{
			CodexVersion: version, BearerToken: token,
		})
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.NoError(t, err, output.String())
	t.Cleanup(func() { _ = client.Close() })
	var effective struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, client.Call(ctx, "config/read", map[string]any{
		"cwd": runtimeDir, "includeLayers": false,
	}, &effective), output.String())
	require.Equal(t, profileName, effective.Config["default_permissions"])
	encoded, err := json.Marshal(effective.Config["permissions"])
	require.NoError(t, err)
	visible := string(encoded)
	for _, want := range []string{
		profileName, readPath, `"read"`, writePath, `"write"`, denyPath, `"deny"`,
		agentdSocket, `"allow"`, `"enabled":true`,
	} {
		require.Contains(t, visible, want)
	}

	// config/read only proves the server startup layer. Codex 0.147's remote
	// TUI then sends sandbox="workspace-write" on thread/start, which normally
	// replaces that managed profile before any model tool runs. Exercise the
	// actual remote thread seam through tclaude's enforcing relay and assert the
	// effective thread profile retains the named socket-aware policy.
	experimental := dialLiveCodexAppServerTCP(t, ctx, relayURL, token)
	defer experimental.Close()
	var initialized map[string]any
	liveCodexAppServerCall(t, experimental, 1, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name": "codex-tui", "title": "tclaude live profile probe", "version": version,
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized)
	require.NoError(t, experimental.WriteJSON(map[string]any{
		"method": "initialized", "params": map[string]any{},
	}))
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		ActivePermissionProfile *struct {
			ID      string  `json:"id"`
			Extends *string `json:"extends,omitempty"`
		} `json:"activePermissionProfile"`
	}
	liveCodexAppServerCall(t, experimental, 2, "thread/start", map[string]any{
		"cwd": runtimeDir, "approvalPolicy": "never", "sandbox": "workspace-write",
		"ephemeral": true,
	}, &started)
	require.NotNil(t, started.ActivePermissionProfile, output.String())
	require.Equal(t, profileName, started.ActivePermissionProfile.ID,
		"the remote thread must use the socket-aware profile, not the TUI's legacy workspace sandbox")
	require.NotEmpty(t, started.Thread.ID)
	// Agentd's stable connection does not negotiate experimentalApi. Its typed
	// turns carry no permission override and must inherit the corrected thread
	// profile byte-for-byte; injecting turn/start.permissions here would be
	// rejected by Codex before the turn reached the model-tool path.
	var stableTurn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	require.NoError(t, client.Call(ctx, "turn/start", map[string]any{
		"threadId": started.Thread.ID, "input": []any{},
	}, &stableTurn), output.String())
	require.NotEmpty(t, stableTurn.Turn.ID)
}

func dialLiveCodexAppServerTCP(
	t *testing.T,
	ctx context.Context,
	relayURL string,
	token string,
) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, relayURL, header)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	require.NoError(t, err)
	return conn
}

func liveCodexAppServerCall(
	t *testing.T,
	conn *websocket.Conn,
	id int,
	method string,
	params any,
	result any,
) {
	t.Helper()
	require.NoError(t, conn.WriteJSON(map[string]any{
		"id": id, "method": method, "params": params,
	}))
	for {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, conn.ReadJSON(&message))
		if string(message.ID) != strconv.Itoa(id) {
			continue
		}
		if message.Error != nil {
			t.Fatalf("Codex app-server %s failed (%d): %s", method,
				message.Error.Code, message.Error.Message)
		}
		require.NoError(t, json.Unmarshal(message.Result, result))
		return
	}
}
