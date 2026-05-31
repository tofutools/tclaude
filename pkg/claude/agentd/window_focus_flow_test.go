package agentd_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the bulk window focus/unfocus feature (group-level
// + whole-dashboard triggers, both wired to one selection modal).
//
// The feature focuses or detaches the OS terminal windows of a set of
// agents. It is window-only — it never stops an agent process. The
// actual OS-window effect (raise / open / detach) is not
// unit-testable, so these scenarios swap the two per-agent seams
// (focusAgentWindow / detachAgentWindows) for recorders and assert the
// system-under-test: that POST /api/agent-windows resolves the right
// set of agents per scope (and per the modal's narrowed selection) and
// dispatches the focus/detach path for each.

// winFocusResp mirrors agentd.agentWindowsResp without importing the
// unexported type.
type winFocusResp struct {
	Direction string `json:"direction"`
	Scope     string `json:"scope"`
	Group     string `json:"group"`
	Targeted  int    `json:"targeted"`
	Focused   int    `json:"focused"`
	Detached  int    `json:"detached"`
	NoWindow  int    `json:"no_window"`
	Failed    int    `json:"failed"`
	Agents    []struct {
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
		Outcome string `json:"outcome"`
		Detail  string `json:"detail"`
	} `json:"agents"`
}

// postAgentWindows fires the dashboard's bulk window endpoint with the
// given body and returns the decoded outcome.
func postAgentWindows(t *testing.T, mux http.Handler, body map[string]any) (int, winFocusResp) {
	t.Helper()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/agent-windows", body))
	var resp winFocusResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode agent-windows response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// winOutcomeFor returns the per-agent outcome string for conv, or "".
func winOutcomeFor(resp winFocusResp, conv string) string {
	for _, a := range resp.Agents {
		if a.ConvID == conv {
			return a.Outcome
		}
	}
	return ""
}

// winRecorder records which conv-ids the focus / detach seams were
// dispatched for. Both seams run in parallel goroutines, so every
// access is mutex-guarded. detachReturns lets a scenario script the
// per-conv client count the detach seam reports back (a 0 models an
// agent with no window open).
type winRecorder struct {
	mu            sync.Mutex
	focused       []string
	detached      []string
	detachReturns map[string]int // conv-id → clients to report (default 1)
}

func newWinRecorder() *winRecorder {
	return &winRecorder{detachReturns: map[string]int{}}
}

// installFocus / installDetach swap the production seams for the
// recorder and register the t.Cleanup restore.
func (w *winRecorder) installFocus(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetFocusAgentWindowForTest(func(sess *db.SessionRow) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.focused = append(w.focused, sess.ConvID)
	}))
}

func (w *winRecorder) installDetach(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetDetachAgentWindowsForTest(func(sess *db.SessionRow) (int, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.detached = append(w.detached, sess.ConvID)
		if n, ok := w.detachReturns[sess.ConvID]; ok {
			return n, nil
		}
		return 1, nil // default: one window dismissed
	}))
}

func (w *winRecorder) focusedSet() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]string(nil), w.focused...)
	sort.Strings(out)
	return out
}

func (w *winRecorder) detachedSet() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]string(nil), w.detached...)
	sort.Strings(out)
	return out
}

// Scenario: a group-level focus reaches every ALIVE member of the
// group and dispatches the focus path for each. The endpoint resolves
// the agent set from the group membership server-side — the pure
// scope-resolution path (no explicit "convs" selection).
func TestAgentWindows_GroupScope_FocusesEveryAliveMember(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installFocus(t)

	const group = "tclaude-dev"
	const convA = "wfga-1111-2222-3333-4444"
	const convB = "wfgb-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "worker-a")
	f.HaveConvWithTitle(convB, "worker-b")
	f.HaveAliveSession(convA, "spwn-wfga", "tmux-wfga", "/tmp/wfga")
	f.HaveAliveSession(convB, "spwn-wfgb", "tmux-wfgb", "/tmp/wfgb")
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "focus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, "focus", resp.Direction)
	assert.Equal(t, 2, resp.Targeted, "both alive members were targeted; agents=%+v", resp.Agents)
	assert.Equal(t, 2, resp.Focused)
	assert.Equal(t, 0, resp.Failed)
	assert.Equal(t, []string{convA, convB}, rec.focusedSet(),
		"the focus path must be dispatched for every alive member")
	assert.Equal(t, "focused", winOutcomeFor(resp, convA))
	assert.Equal(t, "focused", winOutcomeFor(resp, convB))
}

// Scenario: a whole-dashboard unfocus reaches every alive agent —
// grouped and ungrouped alike — and detaches each one's windows. The
// agents themselves are never touched: this is the declutter button.
func TestAgentWindows_AllScope_UnfocusHitsGroupedAndUngrouped(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installDetach(t)

	const group = "tclaude-dev"
	const groupedConv = "wfag-1111-2222-3333-4444"
	const looseConv = "wfal-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(groupedConv, "grouped-worker")
	f.HaveConvWithTitle(looseConv, "ungrouped-worker")
	f.HaveAliveSession(groupedConv, "spwn-wfag", "tmux-wfag", "/tmp/wfag")
	f.HaveAliveSession(looseConv, "spwn-wfal", "tmux-wfal", "/tmp/wfal")
	f.HaveMember(group, groupedConv)
	f.HaveEnrolledAgent(looseConv) // ungrouped — on the roster, in no group

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "unfocus", "scope": "all",
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, "unfocus", resp.Direction)
	assert.Equal(t, 2, resp.Targeted,
		"all-scope covers grouped + ungrouped agents; agents=%+v", resp.Agents)
	assert.Equal(t, 2, resp.Detached)
	assert.Equal(t, []string{groupedConv, looseConv}, rec.detachedSet(),
		"the detach path must be dispatched for grouped AND ungrouped agents")

	// Window-only: the agents keep running — both tmux sessions stay
	// alive. unfocus dismisses windows, never the process.
	assert.True(t, f.World.Tmux.IsAlive("tmux-wfag"), "grouped agent keeps running")
	assert.True(t, f.World.Tmux.IsAlive("tmux-wfal"), "ungrouped agent keeps running")
}

// Scenario: the tray's "Unfocus all agents" item detaches every active
// agent's window — grouped and ungrouped alike — without an HTTP
// round-trip. It is the in-process twin of POST /api/agent-windows
// {"direction":"unfocus","scope":"all"}: window-only, no agent process
// is stopped. Covers the path runTrayBlocking's menu handler drives.
func TestUnfocusAllAgentWindows_TrayPath_DetachesEveryActiveAgent(t *testing.T) {
	f := newFlow(t)
	rec := newWinRecorder()
	rec.installDetach(t)

	const group = "tclaude-dev"
	const groupedConv = "wfta-1111-2222-3333-4444"
	const looseConv = "wftl-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(groupedConv, "grouped-worker")
	f.HaveConvWithTitle(looseConv, "ungrouped-worker")
	f.HaveAliveSession(groupedConv, "spwn-wfta", "tmux-wfta", "/tmp/wfta")
	f.HaveAliveSession(looseConv, "spwn-wftl", "tmux-wftl", "/tmp/wftl")
	f.HaveMember(group, groupedConv)
	f.HaveEnrolledAgent(looseConv) // ungrouped — on the roster, in no group

	targeted, detached, noWindow, failed, err := agentd.UnfocusAllAgentWindowsForTest()
	require.NoError(t, err)

	assert.Equal(t, 2, targeted, "all-scope covers grouped + ungrouped agents")
	assert.Equal(t, 2, detached)
	assert.Equal(t, 0, noWindow)
	assert.Equal(t, 0, failed)
	assert.Equal(t, []string{groupedConv, looseConv}, rec.detachedSet(),
		"the detach path must be dispatched for grouped AND ungrouped agents")

	// Window-only: both agents keep running — the tray button declutters
	// windows, it never stops a process.
	assert.True(t, f.World.Tmux.IsAlive("tmux-wfta"), "grouped agent keeps running")
	assert.True(t, f.World.Tmux.IsAlive("tmux-wftl"), "ungrouped agent keeps running")
}

// Scenario: the selection modal narrows the set — the human ticked a
// subset of the group. The endpoint acts on exactly the "convs" list,
// intersected with the scope: a conv outside the group is dropped, so
// the group modal can never reach an agent in another group.
func TestAgentWindows_NarrowedSelectionIntersectsScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installFocus(t)

	const group = "tclaude-dev"
	const convA = "wfna-1111-2222-3333-4444"
	const convB = "wfnb-1111-2222-3333-4444"
	const convC = "wfnc-1111-2222-3333-4444"
	const outsider = "wfno-1111-2222-3333-4444" // alive, but NOT in the group
	f.HaveGroup(group)
	for _, c := range []string{convA, convB, convC, outsider} {
		f.HaveConvWithTitle(c, "w-"+c[:4])
		f.HaveAliveSession(c, "spwn-"+c[:8], "tmux-"+c[:8], "/tmp/"+c[:8])
	}
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)
	f.HaveMember(group, convC)
	f.HaveEnrolledAgent(outsider)

	// Modal selection: A and C ticked, B left unticked — plus an
	// out-of-group conv that a tampered request might smuggle in.
	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "focus", "scope": "group", "group": group,
		"convs": []string{convA, convC, outsider},
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted,
		"only the in-scope ticked agents are acted on; agents=%+v", resp.Agents)
	assert.Equal(t, []string{convA, convC}, rec.focusedSet(),
		"unticked member B and out-of-group outsider must both be excluded")
}

// Scenario: an offline agent in scope is never targeted — the endpoint
// resolves only ALIVE tmux sessions, so an offline member focuses
// nothing and produces no outcome row.
func TestAgentWindows_SkipsOfflineAgents(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installFocus(t)

	const group = "tclaude-dev"
	const aliveConv = "wfoa-1111-2222-3333-4444"
	const offlineConv = "wfoo-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(aliveConv, "running-worker")
	f.HaveConvWithTitle(offlineConv, "offline-worker")
	f.HaveAliveSession(aliveConv, "spwn-wfoa", "tmux-wfoa", "/tmp/wfoa")
	f.HaveMember(group, aliveConv)
	f.HaveMember(group, offlineConv) // member, but never had a live session

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "focus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 1, resp.Targeted, "only the alive member is targeted; agents=%+v", resp.Agents)
	assert.Equal(t, []string{aliveConv}, rec.focusedSet())
	assert.Empty(t, winOutcomeFor(resp, offlineConv),
		"an offline member is never collected, so it has no outcome row")
}

// Scenario: unfocus on an agent whose tmux session has no client
// attached is a clean no-op — the detach path runs, finds zero
// windows, and reports "no_window" rather than failing.
func TestAgentWindows_UnfocusNoWindowIsNoOp(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()

	const group = "tclaude-dev"
	const windowed = "wfww-1111-2222-3333-4444"
	const noWindow = "wfwn-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(windowed, "windowed-worker")
	f.HaveConvWithTitle(noWindow, "headless-worker")
	f.HaveAliveSession(windowed, "spwn-wfww", "tmux-wfww", "/tmp/wfww")
	f.HaveAliveSession(noWindow, "spwn-wfwn", "tmux-wfwn", "/tmp/wfwn")
	f.HaveMember(group, windowed)
	f.HaveMember(group, noWindow)
	// The headless agent reports 0 attached clients.
	rec.detachReturns[noWindow] = 0
	rec.installDetach(t)

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "unfocus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted)
	assert.Equal(t, 1, resp.Detached, "the windowed agent's window was dismissed")
	assert.Equal(t, 1, resp.NoWindow, "the headless agent was a no-op, not a failure")
	assert.Equal(t, 0, resp.Failed)
	assert.Equal(t, "detached", winOutcomeFor(resp, windowed))
	assert.Equal(t, "no_window", winOutcomeFor(resp, noWindow))
}

// Scenario: a malformed request is rejected up front with a 400 — the
// endpoint never half-runs an ambiguous bulk op.
func TestAgentWindows_RejectsBadDirectionAndScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	cases := []map[string]any{
		{"direction": "sideways", "scope": "all"},     // bad direction
		{"direction": "focus", "scope": "everything"}, // bad scope
		{"direction": "focus", "scope": "group"},      // group scope, no group name
		{"scope": "all"},                              // missing direction
	}
	for _, body := range cases {
		rec := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agent-windows", body))
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"body %+v should be rejected; got %d %s", body, rec.Code, rec.Body.String())
	}
}
