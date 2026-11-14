package agentd_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// An agent terminal opened right after a spawn races the launch: `session new`
// writes the session row before it creates the tmux pane. That window is a
// wait, not a failure: the terminal socket says "starting" (4005) and the
// browser redials until the pane is up, then attaches. These drive the
// dashboard's "open window" route while the simulator holds the pane back.

const (
	termCloseStarting   = 4005
	termCloseNotRunning = 4006
)

// attachWaitServer serves the dashboard with the PTY command swapped for a
// program that proves an attach happened, and counts the PTYs it starts.
func attachWaitServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attaches atomic.Int32
	t.Cleanup(agentd.SetTermWSHookForTest(&agentd.TermWSHook{
		RewriteCommand: func(_, tmuxSession string) (string, string) {
			attaches.Add(1)
			return "printf 'ATTACHED\\n'; exec cat", tmuxSession
		},
	}))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	t.Cleanup(server.Close)
	return server, &attaches
}

// dialAgentTerminal opens path and returns the first output, or the close
// code when the socket closes first.
func dialAgentTerminal(t *testing.T, server *httptest.Server, path string) (output string, code int) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+path, http.Header{"Origin": {"http://127.0.0.1:0"}})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (HTTP %d) — a browser sees only a failed handshake", path, err, status)
	}
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":80,"rows":24}`)))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return "", closeErr.Code
			}
			t.Fatalf("read %s: %v", path, err)
		}
		if out := string(data); strings.Contains(out, "ATTACHED") {
			return out, 0
		}
	}
}

// waitForLaunchRow polls until the spawn's session row exists — the moment the
// launch has recorded itself but has no pane yet — and returns its conv-id.
func waitForLaunchRow(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := db.ListSessions()
		require.NoError(t, err)
		for _, row := range rows {
			if row.ConvID != "" {
				return row.ConvID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the spawn never wrote its session row")
	return ""
}

func TestOpenWindowRightAfterSpawnWaitsForThePaneAndAttaches(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	held := make(chan struct{})
	f.World.SpawnPaneHeld = held
	server, attaches := attachWaitServer(t)

	spawned := make(chan testharness.SpawnResp, 1)
	go func() { spawned <- f.SpawnWith("dev", map[string]any{"name": "worker"}) }()
	convID := waitForLaunchRow(t)

	// The row is there, the pane is not: the browser hears "starting", and no
	// attach command ran against a missing pane.
	_, code := dialAgentTerminal(t, server, "/api/open-window-ws/"+convID)
	assert.Equal(t, termCloseStarting, code, "an attach before the pane exists must say the agent is starting")
	assert.Zero(t, attaches.Load(), "nothing attaches before the pane exists")

	close(held)
	resp := <-spawned
	require.Equal(t, http.StatusOK, resp.Code, "spawn: %s", resp.Raw)

	// The browser's next redial, by agent id as the dashboard dials it, attaches.
	out, code := dialAgentTerminal(t, server, "/api/open-window-ws/"+resp.AgentID)
	assert.Zero(t, code, "the redial after the pane came up must attach, not close")
	assert.Contains(t, out, "ATTACHED")
	assert.EqualValues(t, 1, attaches.Load())
}

func TestOpenWindowOnAPaneThatNeverCameUpSaysNotRunning(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	held := make(chan struct{})
	f.World.SpawnPaneHeld = held
	server, attaches := attachWaitServer(t)

	spawned := make(chan testharness.SpawnResp, 1)
	go func() { spawned <- f.SpawnWith("dev", map[string]any{"name": "worker"}) }()
	convID := waitForLaunchRow(t)

	// Past the start window a missing pane is an agent that did not start: a
	// readable "not running", which the browser settles on, not a wait.
	later := time.Now().Add(agentd.TerminalStartWindowForTest + time.Second)
	restore := agentd.SetTerminalNowForTest(func() time.Time { return later })
	_, code := dialAgentTerminal(t, server, "/api/open-window-ws/"+convID)
	restore()
	assert.Equal(t, termCloseNotRunning, code)
	assert.Zero(t, attaches.Load())

	close(held)
	<-spawned
}

// The browser's own attach takes the session (attach -d). Its quiet redial
// after a dropped socket must not: if the operator has since opened the
// terminal somewhere else, the unforced attach declines and the browser hears
// "detached" instead of dragging it back.
func TestOpenWindowQuietRedialDoesNotTakeTheSessionBack(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	spawned := f.Spawn("dev", "worker")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	first := captureWebTermCommand(t, "/api/open-window-ws/"+spawned.ConvID)
	assert.Contains(t, first, "session attach --force", "an operator's attach takes the session")

	redial := captureWebTermCommand(t, "/api/open-window-ws/"+spawned.ConvID+"?reattach=1")
	assert.Contains(t, redial, "session attach ", "still the ordinary attach wrapper")
	assert.NotContains(t, redial, "--force", "a quiet redial never displaces another client")
}
