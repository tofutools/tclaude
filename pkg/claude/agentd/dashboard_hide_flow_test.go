package agentd_test

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the per-agent "hide" button — the inverse of the
// per-agent "focus" button. Hide detaches one agent's tmux client
// (POST /api/hide/{conv}); the agent PROCESS keeps running, only the
// terminal window goes away. It is the "windows" button's bulk-unfocus
// detach, scoped to a single agent row.
//
// The real tmux detach is not unit-testable, so these scenarios swap
// the detachAgentWindows seam (shared with /api/agent-windows) for a
// recorder and assert the system-under-test: that POST /api/hide/{conv}
// resolves the agent, dispatches the detach, maps the client count
// onto the response, treats a zero-client detach as a clean no-op
// rather than an error, and never touches the agent process.

// hideResp mirrors agentd.hideAgentResp without importing the
// unexported type.
type hideResp struct {
	ConvID   string `json:"conv_id"`
	Detached int    `json:"detached"`
}

// postHide fires the per-agent hide endpoint for conv and returns the
// decoded outcome (zero-value resp on a non-200).
func postHide(t *testing.T, mux http.Handler, conv string) (int, hideResp) {
	t.Helper()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/hide/"+conv, nil))
	var resp hideResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode hide response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// Scenario: hiding an attached agent runs the detach path for that one
// agent and reports the dismissed-window count. The agent PROCESS is
// untouched — its tmux session stays alive and the dashboard snapshot
// still reads it online. This is the declutter button, not a stop.
func TestHideAgent_DetachesAttachedAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "hida-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-hida"
	f.HaveConvWithTitle(conv, "windowed-worker")
	f.HaveAliveSession(conv, "spwn-hida", tmuxSes, f.TestCwd("hida"))
	f.HaveEnrolledAgent(conv)

	var mu sync.Mutex
	var detached []string
	t.Cleanup(agentd.SetDetachAgentWindowsForTest(func(sess *db.SessionRow) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		detached = append(detached, sess.ConvID)
		return 1, nil // one window dismissed
	}))

	code, resp := postHide(t, mux, conv)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, conv, resp.ConvID)
	assert.Equal(t, 1, resp.Detached, "the attached window was detached")
	assert.Equal(t, []string{conv}, detached, "the detach path ran for exactly this agent")

	// Window-only: the agent keeps running. Its tmux session is alive
	// and the dashboard still reads it online.
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes), "the agent process is untouched")
	snap := fetchDashSnapshot(t, mux)
	a := findDashAgent(snap, conv)
	require.NotNil(t, a, "the agent stays on the roster after a hide")
	assert.True(t, a.Online, "hide detaches the window only — the agent stays online")
}

// Scenario: hide is idempotent. The first click detaches the live
// window; a second click on the now-detached agent finds zero clients
// and is a clean no-op — 200 with detached:0, never an error. The
// agent stays alive across both clicks.
func TestHideAgent_IdempotentNoOpForAlreadyDetached(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "hidi-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-hidi"
	f.HaveConvWithTitle(conv, "detach-me-twice")
	f.HaveAliveSession(conv, "spwn-hidi", tmuxSes, f.TestCwd("hidi"))
	f.HaveEnrolledAgent(conv)

	// Model session.DetachSessionClients: the first detach dismisses
	// the one attached client, every later detach finds none.
	var mu sync.Mutex
	calls := 0
	t.Cleanup(agentd.SetDetachAgentWindowsForTest(func(*db.SessionRow) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 1, nil // first click: one window detached
		}
		return 0, nil // already detached: clean no-op
	}))

	// First click — detaches the live window.
	code, resp := postHide(t, mux, conv)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 1, resp.Detached, "the first hide detaches the open window")

	// Second click — the agent is already hidden. Still a 200, just a
	// zero-count no-op; the endpoint never errors on a re-hide.
	code, resp = postHide(t, mux, conv)
	require.Equal(t, http.StatusOK, code, "re-hiding an already-hidden agent must not error")
	assert.Equal(t, 0, resp.Detached, "the second hide is an idempotent no-op")

	// The agent process is untouched by either click.
	assert.True(t, f.World.Tmux.IsAlive(tmuxSes), "the agent keeps running across both hides")
}

// Scenario: hiding an offline agent — one with no live tmux session —
// is a 404. There is no window to detach, and the boundary matches the
// per-agent focus endpoint (POST /api/jump/{conv}).
func TestHideAgent_OfflineAgentIs404(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "hido-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "offline-worker") // indexed, but never had a live session

	var dispatched bool
	t.Cleanup(agentd.SetDetachAgentWindowsForTest(func(*db.SessionRow) (int, error) {
		dispatched = true
		return 0, nil
	}))

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/hide/"+conv, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"an offline agent has no window to hide; body=%s", rec.Body.String())
	assert.False(t, dispatched, "the detach path must not run for an offline agent")
}

// Scenario: the hide endpoint is POST-only — a GET is rejected with a
// 405 and never runs the detach.
func TestHideAgent_RejectsNonPost(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "hidm-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "method-worker")
	f.HaveAliveSession(conv, "spwn-hidm", "tmux-hidm", f.TestCwd("hidm"))

	var dispatched bool
	t.Cleanup(agentd.SetDetachAgentWindowsForTest(func(*db.SessionRow) (int, error) {
		dispatched = true
		return 0, nil
	}))

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodGet, "/api/hide/"+conv, nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"GET must be rejected; body=%s", rec.Body.String())
	assert.False(t, dispatched, "a rejected method must not run the detach")
}
