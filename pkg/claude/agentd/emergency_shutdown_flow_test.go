package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the emergency-shutdown buttons (group level +
// whole-dashboard level). The feature stops a scope of running agents
// fast, escalating per agent: inject /exit (soft), wait a grace
// window, then force-kill any agent still alive. It is stop-only — no
// conversation, enrollment, group membership or permission is deleted.
//
// These scenarios drive POST /api/emergency-shutdown on the dashboard
// mux — the same surface the browser buttons hit — and pass a tiny
// grace_ms so the escalation never sleeps for real seconds. Liveness
// is asserted at the real surface (TmuxSim.IsAlive) and the survival
// of group/enrollment data via the dashboard snapshot.

// emShutdownResp mirrors agentd.emergencyShutdownResp without
// importing the unexported type.
type emShutdownResp struct {
	Scope            string `json:"scope"`
	Group            string `json:"group"`
	GraceMs          int64  `json:"grace_ms"`
	Targeted         int    `json:"targeted"`
	ExitedGracefully int    `json:"exited_gracefully"`
	ForceKilled      int    `json:"force_killed"`
	AlreadyOffline   int    `json:"already_offline"`
	Failed           int    `json:"failed"`
	Agents           []struct {
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
		Outcome string `json:"outcome"`
		Detail  string `json:"detail"`
	} `json:"agents"`
}

// postEmergencyShutdown fires the dashboard's emergency-shutdown
// endpoint with the given body and returns the decoded outcome.
func postEmergencyShutdown(t *testing.T, mux http.Handler, body map[string]any) (int, emShutdownResp) {
	t.Helper()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/emergency-shutdown", body))
	var resp emShutdownResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode emergency-shutdown response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// outcomeFor returns the per-agent outcome string for conv, or "" if
// conv was not in the response (e.g. it was offline and never
// targeted).
func outcomeFor(resp emShutdownResp, conv string) string {
	for _, a := range resp.Agents {
		if a.ConvID == conv {
			return a.Outcome
		}
	}
	return ""
}

// groupInSnap returns the dashboard-snapshot view of group `name`, or
// nil — used to assert a group (and its membership) survived a
// stop-only op.
func groupInSnap(snap dashSnapshot, name string) *dashGroup {
	for i := range snap.Groups {
		if snap.Groups[i].Name == name {
			return &snap.Groups[i]
		}
	}
	return nil
}

// hangOnExit makes the agent behind convID ignore /exit: it writes a
// turn but never flips alive, so a soft /exit can't bring it down.
// This encodes the "hung agent" quirk in the simulator — the case the
// emergency-shutdown escalation must catch and force-kill.
func hangOnExit(t *testing.T, f *testharness.Flow, convID string) {
	t.Helper()
	cc := f.World.CCs.GetByConvID(convID)
	require.NotNil(t, cc, "no CCSim registered for %s", convID)
	cc.OnInput("/exit", func(c *testharness.CCSim, line string) bool {
		_ = c.WriteUserTurn("[hung agent: /exit ignored]")
		// Consume the line — do NOT fall through to the default /exit
		// handler, which would MarkDead. A hung agent stays alive.
		return true
	})
}

// Scenario: a group-level emergency shutdown escalates ONLY the agent
// that ignores /exit. The agent that honours /exit exits gracefully
// and is never force-killed; the hung agent is force-killed once the
// grace window closes. Group membership and enrollment survive — the
// op is stop-only.
func TestEmergencyShutdown_GroupScope_ForceKillsOnlyTheHungAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const goodConv = "esgd-1111-2222-3333-4444"
	const hungConv = "eshu-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(goodConv, "graceful-worker")
	f.HaveConvWithTitle(hungConv, "hung-worker")
	f.HaveAliveSession(goodConv, "spwn-esgd", "tmux-esgd", "/tmp/esgd")
	f.HaveAliveSession(hungConv, "spwn-eshu", "tmux-eshu", "/tmp/eshu")
	f.HaveMember(group, goodConv)
	f.HaveMember(group, hungConv)
	hangOnExit(t, f, hungConv)
	require.True(t, f.World.Tmux.IsAlive("tmux-esgd"), "pre: graceful agent is alive")
	require.True(t, f.World.Tmux.IsAlive("tmux-eshu"), "pre: hung agent is alive")

	code, resp := postEmergencyShutdown(t, mux, map[string]any{
		"scope": "group", "group": group, "grace_ms": 60,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted, "both alive members were targeted")
	assert.Equal(t, 1, resp.ExitedGracefully, "one agent honoured /exit; agents=%+v", resp.Agents)
	assert.Equal(t, 1, resp.ForceKilled, "one agent was hung; agents=%+v", resp.Agents)
	assert.Equal(t, 0, resp.Failed, "agents=%+v", resp.Agents)
	assert.Equal(t, "exited_gracefully", outcomeFor(resp, goodConv),
		"an agent that exits on /exit must NOT be force-killed")
	assert.Equal(t, "force_killed", outcomeFor(resp, hungConv),
		"an agent that ignores /exit must be force-killed after the grace window")

	assert.False(t, f.World.Tmux.IsAlive("tmux-esgd"), "graceful agent's session is gone")
	assert.False(t, f.World.Tmux.IsAlive("tmux-eshu"), "hung agent's session was force-killed")

	// Stop-only: emergency shutdown ends sessions, never data. Group
	// membership and the active roster survive, so every shut-down
	// agent can be brought back by simply resuming its session.
	snap := fetchDashSnapshot(t, mux)
	dg := groupInSnap(snap, group)
	require.NotNil(t, dg, "the group itself must survive a stop-only op")
	assert.Len(t, dg.Members, 2, "both members stay enrolled in the group")
	assert.True(t, agentInSnap(snap.Agents, goodConv), "graceful agent stays on the roster")
	assert.True(t, agentInSnap(snap.Agents, hungConv), "hung agent stays on the roster")
}

// Scenario: a whole-dashboard emergency shutdown reaches every alive
// agent — grouped and ungrouped alike — and escalates each
// independently. No self to exclude: the request comes from the
// human's browser.
func TestEmergencyShutdown_AllScope_HitsGroupedAndUngrouped(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const groupedConv = "esag-1111-2222-3333-4444" // grouped, honours /exit
	const looseConv = "esal-1111-2222-3333-4444"   // ungrouped, hung
	f.HaveGroup(group)
	f.HaveConvWithTitle(groupedConv, "grouped-worker")
	f.HaveConvWithTitle(looseConv, "ungrouped-worker")
	f.HaveAliveSession(groupedConv, "spwn-esag", "tmux-esag", "/tmp/esag")
	f.HaveAliveSession(looseConv, "spwn-esal", "tmux-esal", "/tmp/esal")
	f.HaveMember(group, groupedConv)
	f.HaveEnrolledAgent(looseConv) // an ungrouped agent — on the roster, in no group
	hangOnExit(t, f, looseConv)

	code, resp := postEmergencyShutdown(t, mux, map[string]any{
		"scope": "all", "grace_ms": 60,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted,
		"all-scope covers grouped + ungrouped agents; agents=%+v", resp.Agents)
	assert.Equal(t, "exited_gracefully", outcomeFor(resp, groupedConv),
		"the grouped agent honoured /exit")
	assert.Equal(t, "force_killed", outcomeFor(resp, looseConv),
		"the ungrouped hung agent was force-killed")
	assert.Equal(t, 1, resp.ExitedGracefully)
	assert.Equal(t, 1, resp.ForceKilled)

	assert.False(t, f.World.Tmux.IsAlive("tmux-esag"), "grouped agent stopped")
	assert.False(t, f.World.Tmux.IsAlive("tmux-esal"), "ungrouped agent stopped")
}

// Scenario: an offline agent in scope is never targeted — emergency
// shutdown collects only ALIVE sessions. A group with one running and
// one offline member shuts down exactly the running one.
func TestEmergencyShutdown_SkipsOfflineAgents(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const aliveConv = "esoa-1111-2222-3333-4444"
	const offlineConv = "esof-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(aliveConv, "running-worker")
	f.HaveConvWithTitle(offlineConv, "offline-worker")
	f.HaveAliveSession(aliveConv, "spwn-esoa", "tmux-esoa", "/tmp/esoa")
	f.HaveMember(group, aliveConv)
	f.HaveMember(group, offlineConv) // member, but never had a live session

	code, resp := postEmergencyShutdown(t, mux, map[string]any{
		"scope": "group", "group": group, "grace_ms": 60,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 1, resp.Targeted, "only the alive member is targeted; agents=%+v", resp.Agents)
	assert.Equal(t, "exited_gracefully", outcomeFor(resp, aliveConv))
	assert.Empty(t, outcomeFor(resp, offlineConv),
		"an offline member is never collected, so it has no outcome row")
	assert.False(t, f.World.Tmux.IsAlive("tmux-esoa"), "the running member was stopped")

	// The offline member is still a group member afterwards — nothing
	// was deleted.
	snap := fetchDashSnapshot(t, mux)
	dg := groupInSnap(snap, group)
	require.NotNil(t, dg)
	assert.Len(t, dg.Members, 2, "both members remain in the group")
}

// Scenario: a malformed scope is rejected up front with a 400 — the
// endpoint never half-runs an ambiguous request.
func TestEmergencyShutdown_RejectsUnknownScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/emergency-shutdown",
			map[string]any{"scope": "everything"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// scope=group with no group name is likewise refused.
	rec = testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/emergency-shutdown",
			map[string]any{"scope": "group"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}
