package agentd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestResolveTUIOperatorTokenOverridesLocalDetection(t *testing.T) {
	t.Setenv(agent.HumanTokenEnvVar, "local-token")

	direct, err := resolveTUIOperatorToken(
		&tuiDashboardParams{OperatorToken: " explicit-token "},
		true,
		false,
	)
	require.NoError(t, err)
	assert.Equal(t, "explicit-token", direct)

	fallback, err := resolveTUIOperatorToken(&tuiDashboardParams{}, false, false)
	require.NoError(t, err)
	assert.Equal(t, "local-token", fallback)

	_, err = resolveTUIOperatorToken(
		&tuiDashboardParams{OperatorToken: "a", RemoteOperatorToken: "host:/token"},
		true,
		true,
	)
	assert.ErrorContains(t, err, "mutually exclusive")

	_, err = resolveTUIOperatorToken(&tuiDashboardParams{}, true, false)
	assert.ErrorContains(t, err, "--operator-token is empty")
}

func TestResolveTUIOperatorTokenReadsExplicitSSHSource(t *testing.T) {
	previous := runTUIOperatorTokenSSH
	t.Cleanup(func() { runTUIOperatorTokenSSH = previous })
	var destination, path string
	runTUIOperatorTokenSSH = func(gotDestination, gotPath string) ([]byte, error) {
		destination, path = gotDestination, gotPath
		return []byte(" remote-token\n"), nil
	}

	token, err := resolveTUIOperatorToken(
		&tuiDashboardParams{RemoteOperatorToken: "operator@agent-host:/srv/tclaude/operator_token"},
		false,
		true,
	)
	require.NoError(t, err)
	assert.Equal(t, "remote-token", token)
	assert.Equal(t, "operator@agent-host", destination)
	assert.Equal(t, "/srv/tclaude/operator_token", path)
}

func TestParseRemoteTUIOperatorTokenSource(t *testing.T) {
	for _, tc := range []struct {
		source      string
		destination string
		path        string
	}{
		{"operator@host:/var/lib/tclaude/token", "operator@host", "/var/lib/tclaude/token"},
		{"operator@host/home/operator/.tclaude/operator_token", "operator@host", "/home/operator/.tclaude/operator_token"},
	} {
		destination, path, err := parseRemoteTUIOperatorTokenSource(tc.source)
		require.NoError(t, err)
		assert.Equal(t, tc.destination, destination)
		assert.Equal(t, tc.path, path)
	}

	for _, source := range []string{
		"",
		"host",
		"-oProxyCommand=bad:/token",
		"host:relative",
		"host:/token\nnext",
	} {
		_, _, err := parseRemoteTUIOperatorTokenSource(source)
		assert.Error(t, err, source)
	}
}

func TestNormalizeTUIConnectURL(t *testing.T) {
	t.Run("host and port default to HTTP", func(t *testing.T) {
		base, origin, err := normalizeTUIConnectURL("agent-host:8321")
		require.NoError(t, err)
		assert.Equal(t, "http://agent-host:8321", base)
		assert.Equal(t, base, origin)
	})

	t.Run("HTTPS URL keeps a reverse-proxy prefix", func(t *testing.T) {
		base, origin, err := normalizeTUIConnectURL("https://agents.example.test/tclaude/")
		require.NoError(t, err)
		assert.Equal(t, "https://agents.example.test/tclaude", base)
		assert.Equal(t, "https://agents.example.test", origin)
	})

	for name, target := range map[string]string{
		"empty":        " ",
		"wrong scheme": "ssh://agent-host",
		"userinfo":     "https://operator@agent-host",
		"query":        "https://agent-host?token=nope",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := normalizeTUIConnectURL(target)
			require.Error(t, err)
		})
	}
}

func TestRemoteTUIAPIUsesOperatorTokenThenRetainsSessionCookie(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tui/v1/peers", r.URL.Path)
		assert.Equal(t, srvOrigin(r), r.Header.Get("Origin"))
		assert.Equal(t, "tclo_remote-test", r.Header.Get(agent.HumanTokenHeader))
		switch calls.Add(1) {
		case 1:
			assert.Empty(t, r.Cookies())
			http.SetCookie(w, &http.Cookie{Name: "tclaude_test", Value: "session", Path: "/"})
		case 2:
			cookie, err := r.Cookie("tclaude_test")
			require.NoError(t, err)
			assert.Equal(t, "session", cookie.Value)
		default:
			t.Fatalf("unexpected call")
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	var rows []tuiAgentRow
	require.NoError(t, api.get("/v1/peers", &rows))
	require.NoError(t, api.get("/v1/peers", &rows))
	assert.Equal(t, int32(2), calls.Load())
}

func TestRemoteTUIAttachStreamsTheDashboardTerminalWebSocket(t *testing.T) {
	var attached atomic.Bool
	var gotInput atomic.Bool
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tui/v1/peers":
			assert.Equal(t, "tclo_remote-test", r.Header.Get(agent.HumanTokenHeader))
			http.SetCookie(w, &http.Cookie{Name: "dash", Value: "session", Path: "/"})
			_, _ = w.Write([]byte(`[]`))
		case "/api/tui/attach-ws/c1":
			attached.Store(true)
			assert.Equal(t, srvOrigin(r), r.Header.Get("Origin"))
			cookie, err := r.Cookie("dash")
			require.NoError(t, err)
			assert.Equal(t, "session", cookie.Value)
			assert.Empty(t, r.Header.Get(agent.HumanTokenHeader),
				"the operator token must not be copied into the terminal upgrade")
			conn, err := upgrader.Upgrade(w, r, nil)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("REMOTE\r\n")))
			messageType, data, err := conn.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, websocket.BinaryMessage, messageType)
			assert.Equal(t, []byte("hello"), data)
			gotInput.Store(true)
			require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("DONE\r\n")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	require.NoError(t, api.get("/v1/peers", &[]tuiAgentRow{}),
		"the ordinary dashboard poll bootstraps the session cookie used by the websocket")

	stdin, inputWriter, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()
	defer func() { _ = inputWriter.Close() }()
	var stdout bytes.Buffer
	command := &remoteTUIAttachCommand{
		api:       api,
		agentName: "alice",
		convID:    "c1",
		stdin:     stdin,
		stdout:    &stdout,
	}
	go func() {
		_, _ = inputWriter.Write([]byte("hello"))
	}()
	require.NoError(t, command.Run())
	assert.True(t, attached.Load())
	assert.True(t, gotInput.Load())
	assert.Equal(t, "REMOTE\r\nDONE\r\n", stdout.String())
}

func TestRemoteTUIAttachClientEscapeClosesOnlyTheStream(t *testing.T) {
	received := make(chan []byte, 1)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(_ *http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tui/attach-ws/c1" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		var input bytes.Buffer
		for {
			messageType, data, readErr := conn.ReadMessage()
			if readErr != nil {
				received <- append([]byte(nil), input.Bytes()...)
				return
			}
			if messageType == websocket.BinaryMessage {
				_, _ = input.Write(data)
			}
		}
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	stdin, inputWriter, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()
	defer func() { _ = inputWriter.Close() }()
	command := &remoteTUIAttachCommand{
		api:       api,
		agentName: "alice",
		convID:    "c1",
		stdin:     stdin,
		stdout:    io.Discard,
	}
	go func() {
		_, _ = inputWriter.Write([]byte{
			'b', 'e', 'f', 'o', 'r', 'e',
			remoteTUIEscapeByte, remoteTUIEscapeByte,
			'a', 'f', 't', 'e', 'r',
			remoteTUIEscapeByte, remoteTUIDetachCommand,
			'i', 'g', 'n', 'o', 'r', 'e', 'd',
		})
	}()

	require.NoError(t, command.Run())
	select {
	case input := <-received:
		assert.Equal(t, append([]byte("before"), append([]byte{remoteTUIEscapeByte}, []byte("after")...)...), input,
			"doubled escape is quoted and bytes after detach are discarded")
	case <-time.After(time.Second):
		t.Fatal("remote terminal server did not observe the client detach")
	}
}

func TestRemoteTUIInputCarriesEscapeStateAcrossReads(t *testing.T) {
	output, detach, pending := remoteTUIInput([]byte{'x', remoteTUIEscapeByte}, false)
	assert.Equal(t, []byte("x"), output)
	assert.False(t, detach)
	assert.True(t, pending)

	output, detach, pending = remoteTUIInput([]byte{'q', remoteTUIEscapeByte, 'D', 'z'}, pending)
	assert.Equal(t, []byte{remoteTUIEscapeByte, 'q'}, output,
		"an unknown escape command remains transparent")
	assert.True(t, detach)
	assert.False(t, pending)
}

func srvOrigin(r *http.Request) string {
	return "http://" + r.Host
}

func TestRemoteTUIAPIRemainsReusableAfterAnOutage(t *testing.T) {
	var available atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !available.Load() {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`[{"conv_id":"c1","title":"back","online":true}]`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	var rows []tuiAgentRow
	require.Error(t, api.get("/v1/peers", &rows))

	available.Store(true)
	require.NoError(t, api.get("/v1/peers", &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "back", rows[0].Title)
}

func TestRemoteTUIAPIRetriesALostMutationResponseWithTheSameIdentity(t *testing.T) {
	var committed atomic.Int32
	var keys, digests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(agent.IdempotencyKeyHeader))
		digests = append(digests, r.Header.Get(agent.RequestDigestHeader))
		if committed.CompareAndSwap(0, 1) {
			// Model "the spawn committed, then agentd restarted before the
			// response reached the client." The retried idempotency key would
			// retrieve the recorded response from production middleware.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack committed response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"group":"dev","agent_id":"agt_once"}`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	api.mutationRetryBackoff = []time.Duration{0}
	var resp agent.SpawnResponse
	require.NoError(t, api.post("/v1/groups/dev/spawn",
		agent.SpawnRequest{Name: "once"}, &resp))

	assert.Equal(t, int32(1), committed.Load())
	assert.Equal(t, "agt_once", resp.AgentID)
	require.Len(t, keys, 2)
	assert.NotEmpty(t, keys[0])
	assert.Equal(t, keys[0], keys[1])
	require.Len(t, digests, 2)
	assert.NotEmpty(t, digests[0])
	assert.Equal(t, digests[0], digests[1])
}

func TestRemoteTUIAPIRetriesAPartialMutationResponseWithTheSameIdentity(t *testing.T) {
	var committed atomic.Int32
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(agent.IdempotencyKeyHeader))
		if committed.CompareAndSwap(0, 1) {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack partial response: %v", err)
				return
			}
			// Headers arrived and the body began, but Content-Length proves it
			// was truncated. The retry must carry the same mutation identity.
			_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"group\":\"dev\"")
			_ = rw.Flush()
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"group":"dev","agent_id":"agt_once"}`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	api.mutationRetryBackoff = []time.Duration{0}
	var resp agent.SpawnResponse
	require.NoError(t, api.post("/v1/groups/dev/spawn",
		agent.SpawnRequest{Name: "once"}, &resp))
	assert.Equal(t, "agt_once", resp.AgentID)
	require.Len(t, keys, 2)
	assert.Equal(t, keys[0], keys[1])
}

func TestRemoteTUIAPIMarksIdempotencyUnknownForReconciliation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"previous daemon exited before recording the response","code":"idempotency_unknown"}`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	err = api.post("/v1/groups/dev/spawn", agent.SpawnRequest{Name: "once"}, &agent.SpawnResponse{})
	require.Error(t, err)
	var ambiguous *tuiAmbiguousMutationError
	assert.True(t, errors.As(err, &ambiguous), err)
	assert.Contains(t, err.Error(), "outcome")
}

func TestRemoteTUIAPIMarksMalformedMutationResponseForReconciliation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	err = api.post("/v1/groups/dev/spawn", agent.SpawnRequest{Name: "once"}, &agent.SpawnResponse{})
	require.Error(t, err)
	var ambiguous *tuiAmbiguousMutationError
	assert.True(t, errors.As(err, &ambiguous), err)
}

func TestTUIInitialRefreshDoesNotOverlapTheFirstTick(t *testing.T) {
	api, err := newRemoteTUIAPI("agent-host:8321", "tclo_remote-test")
	require.NoError(t, err)
	m := newTUIModel(api)
	assert.True(t, m.refreshing, "Init starts a refresh immediately")

	updated, cmd := m.Update(tuiTickMsg{})
	got := updated.(tuiModel)
	assert.True(t, got.refreshing)
	assert.NotNil(t, cmd, "the tick must reschedule itself without replacing the in-flight refresh")
}

func TestRemoteTUIModelStreamsAttachButDoesNotClaimOtherHostLocalCapabilities(t *testing.T) {
	api, err := newRemoteTUIAPI("agent-host:8321", "tclo_remote-test")
	require.NoError(t, err)
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "live", Online: true}}

	assert.True(t, m.operator)
	assert.Equal(t, "enter remote attach", m.enterHint())
	assert.Contains(t, m.renderSpawnForm(), "go to its pane")
	assert.NotContains(t, m.renderSpawnForm(), "tab complete dir")
	assert.Contains(t, m.renderHelp(), "authenticated terminal WebSocket")

	updated, _ := m.handleKey(tea.KeyPressMsg{Code: 'q', Text: "q"})
	remote := updated.(tuiModel)
	assert.Contains(t, remote.confirmPrompt(), "Quit this remote console?")
	assert.NotContains(t, remote.confirmPrompt(), "shut down agentd")
	assert.Contains(t, remote.renderHelp(), "agentd keeps running")
	assert.Contains(t, remote.renderHelp(), "reconnects when agentd returns")
}

func TestTUIAmbiguousMutationForcesReconciliation(t *testing.T) {
	m := newTUIModel(nil)
	m.spawning = true
	updated, cmd := m.Update(tuiSpawnedMsg{
		group: "dev",
		err: &tuiAmbiguousMutationError{
			err:      io.ErrUnexpectedEOF,
			attempts: 2,
		},
	})
	got := updated.(tuiModel)
	assert.False(t, got.spawning)
	assert.True(t, got.refreshing)
	assert.True(t, got.reconcilingMutation)
	assert.Contains(t, got.notice, "outcome unknown")
	assert.Contains(t, got.notice, "refreshing")
	assert.NotNil(t, cmd)

	got.agents = []tuiAgentRow{{ConvID: "c1", Title: "offline", Online: false}}
	for _, key := range []tea.KeyPressMsg{
		{Code: 'n', Text: "n"},
		{Code: 'N', Text: "N"},
		{Code: tea.KeyDelete},
		{Code: tea.KeyEnter},
	} {
		blocked, mutationCmd := got.handleKey(key)
		got = blocked.(tuiModel)
		assert.Nil(t, mutationCmd)
		assert.Equal(t, tuiModeList, got.mode)
		assert.False(t, got.spawning)
		assert.False(t, got.retiring)
		assert.False(t, got.resuming)
		assert.Contains(t, got.notice, "successful refresh")
	}

	got.mode = tuiModeSpawn
	blocked, mutationCmd := got.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	got = blocked.(tuiModel)
	assert.Nil(t, mutationCmd)
	assert.Equal(t, tuiModeList, got.mode)
	assert.False(t, got.spawning)

	got.mode = tuiModeConfirmRetire
	got.lifecycleTarget = tuiAgentRow{ConvID: "c1", Title: "offline"}
	blocked, mutationCmd = got.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	got = blocked.(tuiModel)
	assert.Nil(t, mutationCmd)
	assert.Equal(t, tuiModeList, got.mode)
	assert.Empty(t, got.lifecycleTarget)
	assert.False(t, got.retiring)

	assert.NotContains(t, got.keyHintLine(), "new agent")
	assert.NotContains(t, got.keyHintLine(), "retire")

	failed, _ := got.Update(tuiDataMsg{err: errors.New("still down")})
	got = failed.(tuiModel)
	assert.True(t, got.reconcilingMutation, "a failed poll cannot settle the mutation")

	settled, _ := got.Update(tuiDataMsg{})
	got = settled.(tuiModel)
	assert.False(t, got.reconcilingMutation)
	assert.Contains(t, got.keyHintLine(), "new agent")
}

func TestTUIOlderRefreshCannotSettleAmbiguousMutation(t *testing.T) {
	m := newTUIModel(nil)
	m.agents = []tuiAgentRow{{ConvID: "current", Title: "current"}}

	updated, _ := m.Update(tuiSpawnedMsg{
		group: "dev",
		err:   &tuiAmbiguousMutationError{err: io.ErrUnexpectedEOF, attempts: 2},
	})
	got := updated.(tuiModel)
	reconciliationGeneration := got.reconciliationRefreshGen
	require.Greater(t, reconciliationGeneration, uint64(1))

	// This poll began before the ambiguous mutation. Even though it succeeds
	// after the mutation response arrives, its view of daemon state is too old
	// to reconcile the outcome.
	stale, _ := got.Update(tuiDataMsg{
		refreshGeneration: reconciliationGeneration - 1,
		agents:            []tuiAgentRow{{ConvID: "stale", Title: "stale"}},
	})
	got = stale.(tuiModel)
	assert.True(t, got.reconcilingMutation)
	assert.True(t, got.refreshing)
	assert.Equal(t, "current", got.agents[0].ConvID)

	settled, _ := got.Update(tuiDataMsg{refreshGeneration: reconciliationGeneration})
	got = settled.(tuiModel)
	assert.False(t, got.reconcilingMutation)
	assert.False(t, got.refreshing)
}

func TestRemoteTUIAPINamesUnsupportedServers(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)

	err = api.get("/v1/peers", &[]tuiAgentRow{})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "without remote TUI support"), err)
}

func TestRemoteTUIAPINeverForwardsTheOperatorTokenThroughARedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		assert.Empty(t, r.Header.Get(agent.HumanTokenHeader))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	api, err := newRemoteTUIAPI(source.URL, "tclo_remote-test")
	require.NoError(t, err)
	err = api.get("/v1/peers", &[]tuiAgentRow{})
	require.Error(t, err)
	assert.False(t, redirected.Load(), "the client must not visit a redirect target at all")
	assert.Contains(t, err.Error(), "Temporary Redirect")
}
