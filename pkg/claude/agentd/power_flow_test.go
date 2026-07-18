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

// Flow coverage for the dashboard's power buttons — the matched pair
// of bulk controls, each at group level and whole-dashboard level:
//
//   - Shutdown  (POST /api/shutdown)  — stops a scope of running
//     agents, escalating per agent: inject /exit (soft), wait a grace
//     window, then force-kill any agent still alive. Stop-only — no
//     conversation, enrollment, group membership or permission is
//     deleted.
//   - Power On  (POST /api/power-on) — the inverse: resumes every
//     OFFLINE agent in scope into a fresh tmux session. Resume-only —
//     it reuses resumeOneConv and creates nothing new.
//
// These scenarios drive the two endpoints on the dashboard mux — the
// same surface the browser buttons hit. Shutdown passes a tiny
// grace_ms so the escalation never sleeps for real seconds. Liveness
// is asserted at real surfaces (TmuxSim.IsAlive, the dashboard
// snapshot) and the survival of group/enrollment data via the
// snapshot.

// powerOutcome mirrors agentd.powerAgentOutcome — one agent's result
// in a shutdown or power-on response.
type powerOutcome struct {
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// shutdownResp mirrors agentd.shutdownResp without importing the
// unexported type.
type shutdownResp struct {
	Scope            string         `json:"scope"`
	Group            string         `json:"group"`
	GraceMs          int64          `json:"grace_ms"`
	Targeted         int            `json:"targeted"`
	ExitedGracefully int            `json:"exited_gracefully"`
	ForceKilled      int            `json:"force_killed"`
	AlreadyOffline   int            `json:"already_offline"`
	Failed           int            `json:"failed"`
	Agents           []powerOutcome `json:"agents"`
}

// powerOnResp mirrors agentd.powerOnResp.
type powerOnResp struct {
	Scope         string         `json:"scope"`
	Group         string         `json:"group"`
	Targeted      int            `json:"targeted"`
	Resumed       int            `json:"resumed"`
	AlreadyOnline int            `json:"already_online"`
	Failed        int            `json:"failed"`
	Agents        []powerOutcome `json:"agents"`
}

// postShutdown fires the dashboard's shutdown endpoint with the given
// body and returns the decoded outcome.
func postShutdown(t *testing.T, mux http.Handler, body map[string]any) (int, shutdownResp) {
	t.Helper()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/shutdown", body))
	var resp shutdownResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode shutdown response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// postPowerOn fires the dashboard's power-on endpoint with the given
// body and returns the decoded outcome.
func postPowerOn(t *testing.T, mux http.Handler, body map[string]any) (int, powerOnResp) {
	t.Helper()
	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/power-on", body))
	var resp powerOnResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode power-on response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// outcomeFor returns the per-agent outcome string for conv, or "" if
// conv was not in the response (e.g. it was already in the desired
// state and never targeted).
func outcomeFor(agents []powerOutcome, conv string) string {
	for _, a := range agents {
		if a.ConvID == conv {
			return a.Outcome
		}
	}
	return ""
}

// groupInSnap returns the dashboard-snapshot view of group `name`, or
// nil — used to assert a group (and its membership) survived a
// stop-only / resume-only op.
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
// shutdown escalation must catch and force-kill.
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

// === Shutdown ========================================================

// Scenario: a group-level shutdown escalates ONLY the agent that
// ignores /exit. The agent that honours /exit exits gracefully and is
// never force-killed; the hung agent is force-killed once the grace
// window closes. Group membership and enrollment survive — the op is
// stop-only.
func TestShutdown_GroupScope_ForceKillsOnlyTheHungAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const goodConv = "esgd-1111-2222-3333-4444"
	const hungConv = "eshu-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(goodConv, "graceful-worker")
	f.HaveConvWithTitle(hungConv, "hung-worker")
	f.HaveAliveSession(goodConv, "spwn-esgd", "tmux-esgd", f.TestCwd("esgd"))
	f.HaveAliveSession(hungConv, "spwn-eshu", "tmux-eshu", f.TestCwd("eshu"))
	f.HaveMember(group, goodConv)
	f.HaveMember(group, hungConv)
	hangOnExit(t, f, hungConv)
	require.True(t, f.World.Tmux.IsAlive("tmux-esgd"), "pre: graceful agent is alive")
	require.True(t, f.World.Tmux.IsAlive("tmux-eshu"), "pre: hung agent is alive")

	code, resp := postShutdown(t, mux, map[string]any{
		"scope": "group", "group": group, "grace_ms": 60,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted, "both alive members were targeted")
	assert.Equal(t, 1, resp.ExitedGracefully, "one agent honoured /exit; agents=%+v", resp.Agents)
	assert.Equal(t, 1, resp.ForceKilled, "one agent was hung; agents=%+v", resp.Agents)
	assert.Equal(t, 0, resp.Failed, "agents=%+v", resp.Agents)
	assert.Equal(t, "exited_gracefully", outcomeFor(resp.Agents, goodConv),
		"an agent that exits on /exit must NOT be force-killed")
	assert.Equal(t, "force_killed", outcomeFor(resp.Agents, hungConv),
		"an agent that ignores /exit must be force-killed after the grace window")

	assert.False(t, f.World.Tmux.IsAlive("tmux-esgd"), "graceful agent's session is gone")
	assert.False(t, f.World.Tmux.IsAlive("tmux-eshu"), "hung agent's session was force-killed")

	// Stop-only: shutdown ends sessions, never data. Group membership
	// and the active roster survive, so every shut-down agent can be
	// brought back by simply resuming its session.
	snap := fetchDashSnapshot(t, mux)
	dg := groupInSnap(snap, group)
	require.NotNil(t, dg, "the group itself must survive a stop-only op")
	assert.Len(t, dg.Members, 2, "both members stay enrolled in the group")
	assert.True(t, agentInSnap(snap.Agents, goodConv), "graceful agent stays on the roster")
	assert.True(t, agentInSnap(snap.Agents, hungConv), "hung agent stays on the roster")
}

// Scenario: a whole-dashboard shutdown reaches every alive agent —
// grouped and ungrouped alike — and escalates each independently. No
// self to exclude: the request comes from the human's browser.
func TestShutdown_AllScope_HitsGroupedAndUngrouped(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const groupedConv = "esag-1111-2222-3333-4444" // grouped, honours /exit
	const looseConv = "esal-1111-2222-3333-4444"   // ungrouped, hung
	f.HaveGroup(group)
	f.HaveConvWithTitle(groupedConv, "grouped-worker")
	f.HaveConvWithTitle(looseConv, "ungrouped-worker")
	f.HaveAliveSession(groupedConv, "spwn-esag", "tmux-esag", f.TestCwd("esag"))
	f.HaveAliveSession(looseConv, "spwn-esal", "tmux-esal", f.TestCwd("esal"))
	f.HaveMember(group, groupedConv)
	f.HaveEnrolledAgent(looseConv) // an ungrouped agent — on the roster, in no group
	hangOnExit(t, f, looseConv)

	code, resp := postShutdown(t, mux, map[string]any{
		"scope": "all", "grace_ms": 60,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted,
		"all-scope covers grouped + ungrouped agents; agents=%+v", resp.Agents)
	assert.Equal(t, "exited_gracefully", outcomeFor(resp.Agents, groupedConv),
		"the grouped agent honoured /exit")
	assert.Equal(t, "force_killed", outcomeFor(resp.Agents, looseConv),
		"the ungrouped hung agent was force-killed")
	assert.Equal(t, 1, resp.ExitedGracefully)
	assert.Equal(t, 1, resp.ForceKilled)

	assert.False(t, f.World.Tmux.IsAlive("tmux-esag"), "grouped agent stopped")
	assert.False(t, f.World.Tmux.IsAlive("tmux-esal"), "ungrouped agent stopped")
}

// Scenario: an offline agent in scope is never targeted — shutdown
// collects only ALIVE sessions. A group with one running and one
// offline member shuts down exactly the running one.
func TestShutdown_SkipsOfflineAgents(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const aliveConv = "esoa-1111-2222-3333-4444"
	const offlineConv = "esof-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(aliveConv, "running-worker")
	f.HaveConvWithTitle(offlineConv, "offline-worker")
	f.HaveAliveSession(aliveConv, "spwn-esoa", "tmux-esoa", f.TestCwd("esoa"))
	f.HaveMember(group, aliveConv)
	f.HaveMember(group, offlineConv) // member, but never had a live session

	code, resp := postShutdown(t, mux, map[string]any{
		"scope": "group", "group": group, "grace_ms": 60,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 1, resp.Targeted, "only the alive member is targeted; agents=%+v", resp.Agents)
	assert.Equal(t, "exited_gracefully", outcomeFor(resp.Agents, aliveConv))
	assert.Empty(t, outcomeFor(resp.Agents, offlineConv),
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
func TestShutdown_RejectsUnknownScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/shutdown",
			map[string]any{"scope": "everything"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// scope=group with no group name is likewise refused.
	rec = testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/shutdown",
			map[string]any{"scope": "group"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// === Power On ========================================================

// Scenario: a group-level power-on resumes ONLY the offline members.
// An already-online member is never collected (mirroring how shutdown
// skips already-offline ones) and so gets no outcome row. The resumed
// agent comes back online; group membership is untouched.
func TestPowerOn_GroupScope_ResumesOfflineSkipsOnline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const offlineConv = "pogo-1111-2222-3333-4444"
	const onlineConv = "pogn-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(offlineConv, "sleeping-worker")
	f.HaveConvWithTitle(onlineConv, "running-worker")
	f.HaveAliveSession(offlineConv, "spwn-pogo", "tmux-pogo", f.World.HomeDir)
	f.HaveAliveSession(onlineConv, "spwn-pogn", "tmux-pogn", f.TestCwd("pogn"))
	f.HaveMember(group, offlineConv)
	f.HaveMember(group, onlineConv)
	// Take one member offline so power-on has something to resume.
	f.MarkOffline("tmux-pogo")
	require.False(t, f.World.Tmux.IsAlive("tmux-pogo"), "pre: one member is offline")
	require.True(t, f.World.Tmux.IsAlive("tmux-pogn"), "pre: the other is online")

	code, resp := postPowerOn(t, mux, map[string]any{
		"scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 1, resp.Targeted, "only the offline member is targeted; agents=%+v", resp.Agents)
	assert.Equal(t, 1, resp.Resumed, "the offline member was resumed; agents=%+v", resp.Agents)
	assert.Equal(t, 0, resp.Failed, "agents=%+v", resp.Agents)
	assert.Equal(t, "resumed", outcomeFor(resp.Agents, offlineConv),
		"an offline member must be resumed")
	assert.Empty(t, outcomeFor(resp.Agents, onlineConv),
		"an already-online member is never collected, so it has no outcome row")

	// The resumed member reads online again; the already-online one is
	// untouched. Resume-only: both stay enrolled in the group.
	snap := fetchDashSnapshot(t, mux)
	resumed := findDashAgent(snap, offlineConv)
	require.NotNil(t, resumed, "the resumed agent stays on the roster")
	assert.True(t, resumed.Online, "the offline member is online after power-on")
	stillUp := findDashAgent(snap, onlineConv)
	require.NotNil(t, stillUp)
	assert.True(t, stillUp.Online, "the already-online member stays online")
	dg := groupInSnap(snap, group)
	require.NotNil(t, dg, "the group survives a resume-only op")
	assert.Len(t, dg.Members, 2, "both members stay enrolled in the group")
}

// Scenario: a whole-dashboard power-on reaches every OFFLINE agent —
// grouped and ungrouped alike — and resumes each. An online agent in
// scope is skipped at collection.
func TestPowerOn_AllScope_HitsGroupedAndUngrouped(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const group = "tclaude-dev"
	const groupedConv = "poag-1111-2222-3333-4444" // grouped, offline
	const looseConv = "poal-1111-2222-3333-4444"   // ungrouped, offline
	const onlineConv = "poan-1111-2222-3333-4444"  // grouped, online → skipped
	f.HaveGroup(group)
	f.HaveConvWithTitle(groupedConv, "grouped-sleeper")
	f.HaveConvWithTitle(looseConv, "ungrouped-sleeper")
	f.HaveConvWithTitle(onlineConv, "grouped-runner")
	f.HaveAliveSession(groupedConv, "spwn-poag", "tmux-poag", f.World.HomeDir)
	f.HaveAliveSession(looseConv, "spwn-poal", "tmux-poal", f.World.HomeDir)
	f.HaveAliveSession(onlineConv, "spwn-poan", "tmux-poan", f.TestCwd("poan"))
	f.HaveMember(group, groupedConv)
	f.HaveMember(group, onlineConv)
	f.HaveEnrolledAgent(looseConv) // an ungrouped agent — on the roster, in no group
	f.MarkOffline("tmux-poag")
	f.MarkOffline("tmux-poal")

	code, resp := postPowerOn(t, mux, map[string]any{"scope": "all"})
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 2, resp.Targeted,
		"all-scope covers the grouped + ungrouped offline agents; agents=%+v", resp.Agents)
	assert.Equal(t, 2, resp.Resumed, "agents=%+v", resp.Agents)
	assert.Equal(t, "resumed", outcomeFor(resp.Agents, groupedConv),
		"the grouped offline agent was resumed")
	assert.Equal(t, "resumed", outcomeFor(resp.Agents, looseConv),
		"the ungrouped offline agent was resumed")
	assert.Empty(t, outcomeFor(resp.Agents, onlineConv),
		"an already-online agent is never collected, so it has no outcome row")

	snap := fetchDashSnapshot(t, mux)
	for _, c := range []string{groupedConv, looseConv, onlineConv} {
		a := findDashAgent(snap, c)
		require.NotNil(t, a, "agent %s on the roster", c)
		assert.True(t, a.Online, "agent %s is online after the all-scope power-on", c)
	}
}

// Scenario: a malformed scope is rejected up front with a 400 — the
// power-on endpoint never half-runs an ambiguous request.
func TestPowerOn_RejectsUnknownScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/power-on",
			map[string]any{"scope": "everything"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// scope=group with no group name is likewise refused.
	rec = testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/power-on",
			map[string]any{"scope": "group"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}
