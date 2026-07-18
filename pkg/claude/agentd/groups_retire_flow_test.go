package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the bulk groups.retire endpoint — the group-level
// parallel of `agent retire`, completing the groups.stop / groups.resume
// lifecycle family. These scenarios drive POST /v1/groups/{name}/retire
// on the SO_PEERCRED-authed mux (f.Mux), the same surface
// `tclaude agent groups retire` hits, and assert at real surfaces:
// enrollment state (db.EnrollmentState), group membership
// (flowGroupHasMember), and tmux liveness (TmuxSim.IsAlive).

// groupRetireResp mirrors the daemon's groupRetireResp without importing
// the unexported type.
type groupRetireResp struct {
	Group   string `json:"group"`
	Action  string `json:"action"`
	Members []struct {
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
		Action  string `json:"action"`
		Detail  string `json:"detail"`
		TmuxSes string `json:"tmux_session"`
	} `json:"members"`
	Warnings []string `json:"warnings"`
}

// postGroupRetire fires POST /v1/groups/{group}/retire with the given
// raw query, routed through peerWrap (AsHumanPeer / an AsAgentPeer
// closure). Decodes the body only on 200.
func postGroupRetire(t *testing.T, mux http.Handler, peerWrap func(*http.Request) *http.Request, group, query string) (int, groupRetireResp) {
	t.Helper()
	path := "/v1/groups/" + group + "/retire"
	if query != "" {
		path += "?" + query
	}
	r := peerWrap(testharness.JSONRequest(t, http.MethodPost, path, nil))
	rec := testharness.Serve(mux, r)
	var resp groupRetireResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode groups retire response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// retireMemberAction returns the per-member action for conv, or "".
func retireMemberAction(resp groupRetireResp, conv string) string {
	for _, m := range resp.Members {
		if m.ConvID == conv {
			return m.Action
		}
	}
	return ""
}

// setConvStatus overrides a live conv's hook status on its session row.
// HaveAliveSession defaults the status to "running"; the ?status= filter
// keys on idle / working / awaiting / error, so the status-filter
// scenarios stamp the status they mean to select against.
func setConvStatus(t *testing.T, convID, status string) {
	t.Helper()
	row, err := db.FindSessionByConvID(convID)
	require.NoError(t, err)
	require.NotNil(t, row, "no session row for %s", convID)
	row.Status = status
	require.NoError(t, db.SaveSession(row))
}

// Scenario: a human bulk-retires a whole group. Every member is demoted
// to a plain conversation (leaves the active roster, drops its group
// membership) and — shutdown defaulting ON — its running pane is
// soft-exited. The conversation data itself survives, so each is
// reinstatable.
func TestGroupRetire_HumanRetiresEveryMember(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const convA = "graa-1111-2222-3333-4444"
	const convB = "grbb-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "worker-a")
	f.HaveConvWithTitle(convB, "worker-b")
	f.HaveAliveSession(convA, "spwn-graa", "tmux-graa", f.TestCwd("graa"))
	f.HaveAliveSession(convB, "spwn-grbb", "tmux-grbb", f.TestCwd("grbb"))
	f.HaveMember(group, convA) // HaveMember enrolls
	f.HaveMember(group, convB)
	require.NoError(t, db.GrantAgentPermission(convA, "self.rename", "human"))

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "retired", retireMemberAction(resp, convA), "members=%+v", resp.Members)
	assert.Equal(t, "retired", retireMemberAction(resp, convB), "members=%+v", resp.Members)

	for _, c := range []string{convA, convB} {
		state, err := db.AgentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.AgentStateRetired, state, "%s must be retired", c)
		assert.False(t, flowGroupHasMember(f, group, c), "%s must leave the group on retire", c)
		// Conversation data survives — the non-destructive half.
		row, err := db.GetConvIndex(c)
		require.NoError(t, err)
		assert.NotNil(t, row, "retire must NOT touch %s's conv_index row", c)
	}
	// Default shutdown ON soft-exits both panes.
	assert.False(t, f.World.Tmux.IsAlive("tmux-graa"), "default shutdown must stop worker-a")
	assert.False(t, f.World.Tmux.IsAlive("tmux-grbb"), "default shutdown must stop worker-b")

	// Grants are revoked as part of the demotion.
	hasPerm, err := db.HasAgentPermissionRow(convA, "self.rename")
	require.NoError(t, err)
	assert.False(t, hasPerm, "retire must revoke permission grants")
}

// Scenario: an agent caller that does NOT hold groups.retire is refused
// with 403 — the slug is the only silent path for an agent on this bulk
// endpoint (no group-owner structural bypass at the bulk level). The
// group's members are left completely untouched.
func TestGroupRetire_AgentWithoutSlugRefused(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const worker = "nswk-1111-2222-3333-4444"
	const caller = "nscl-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "spwn-nswk", "tmux-nswk", f.TestCwd("nswk"))
	f.HaveMember(group, worker)
	// caller is an agent, but holds no groups.retire grant.
	f.HaveConvWithTitle(caller, "ungranted-coordinator")

	wrap := func(r *http.Request) *http.Request { return agentd.AsAgentPeer(r, caller) }
	code, _ := postGroupRetire(t, f.Mux, wrap, group, "")
	require.Equal(t, http.StatusForbidden, code, "an agent without groups.retire must be refused")

	// The worker is untouched: still an active agent, still a member,
	// still online.
	state, err := db.AgentState(worker)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "a refused retire must not demote anyone")
	assert.True(t, flowGroupHasMember(f, group, worker), "membership must survive a refused retire")
	assert.True(t, f.World.Tmux.IsAlive("tmux-nswk"), "a refused retire must not stop sessions")
}

// Scenario: an agent that holds groups.retire retires the OTHER members
// of its group but is itself skipped (skipped:self) — an agent never
// demotes itself out from under the request it is serving. The caller
// stays an active agent and a group member; the workers are retired.
func TestGroupRetire_AgentWithSlugSkipsSelf(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const caller = "sfcl-1111-2222-3333-4444" // the coordinator running the command
	const workerA = "sfwa-1111-2222-3333-4444"
	const workerB = "sfwb-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(caller, "coordinator")
	f.HaveConvWithTitle(workerA, "worker-a")
	f.HaveConvWithTitle(workerB, "worker-b")
	f.HaveAliveSession(caller, "spwn-sfcl", "tmux-sfcl", f.TestCwd("sfcl"))
	f.HaveAliveSession(workerA, "spwn-sfwa", "tmux-sfwa", f.TestCwd("sfwa"))
	f.HaveAliveSession(workerB, "spwn-sfwb", "tmux-sfwb", f.TestCwd("sfwb"))
	f.HaveMember(group, caller)
	f.HaveMember(group, workerA)
	f.HaveMember(group, workerB)
	require.NoError(t, db.GrantAgentPermission(caller, "groups.retire", "human"))

	wrap := func(r *http.Request) *http.Request { return agentd.AsAgentPeer(r, caller) }
	code, resp := postGroupRetire(t, f.Mux, wrap, group, "")
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, "skipped:self", retireMemberAction(resp, caller),
		"the caller must never retire itself; members=%+v", resp.Members)
	assert.Equal(t, "retired", retireMemberAction(resp, workerA), "members=%+v", resp.Members)
	assert.Equal(t, "retired", retireMemberAction(resp, workerB), "members=%+v", resp.Members)

	// The caller is untouched: still active, still a member, still online.
	callerState, err := db.AgentState(caller)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, callerState, "the caller stays an active agent")
	assert.True(t, flowGroupHasMember(f, group, caller), "the caller stays a member")
	assert.True(t, f.World.Tmux.IsAlive("tmux-sfcl"), "the caller's own pane is never /exit'd")

	// The workers are retired and stopped.
	for _, c := range []string{workerA, workerB} {
		state, err := db.AgentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.AgentStateRetired, state, "%s must be retired", c)
	}
	assert.False(t, f.World.Tmux.IsAlive("tmux-sfwa"), "worker-a's pane is soft-exited")
	assert.False(t, f.World.Tmux.IsAlive("tmux-sfwb"), "worker-b's pane is soft-exited")
}

// Scenario: ?shutdown=0 demotes every member but leaves their running
// sessions alive — the bulk twin of `agent retire --no-shutdown`.
func TestGroupRetire_NoShutdownKeepsSessionsAlive(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const conv = "nsdn-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(conv, "kept-worker")
	f.HaveAliveSession(conv, "spwn-nsdn", "tmux-nsdn", f.TestCwd("nsdn"))
	f.HaveMember(group, conv)

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "shutdown=0")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "retired", retireMemberAction(resp, conv), "members=%+v", resp.Members)

	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state, "the member is still demoted")
	assert.True(t, f.World.Tmux.IsAlive("tmux-nsdn"),
		"shutdown=0 must leave the running session alive")
}

// Scenario: retire is idempotent — a member that is already retired (or
// was never an agent) is reported skipped:not_active_agent, not retired
// again, and the call still succeeds for the rest.
func TestGroupRetire_SkipsAlreadyRetiredMember(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const active = "iaac-1111-2222-3333-4444"
	const gone = "iagn-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(active, "still-here")
	f.HaveConvWithTitle(gone, "already-retired")
	f.HaveMember(group, active)
	f.HaveMember(group, gone)
	// Retire `gone` out-of-band so the bulk pass meets a non-active member.
	_, err := db.RetireAgent(gone, "human", "pre-retired")
	require.NoError(t, err)

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "retired", retireMemberAction(resp, active), "members=%+v", resp.Members)
	assert.Equal(t, "skipped:not_active_agent", retireMemberAction(resp, gone),
		"an already-retired member must be skipped, not re-retired; members=%+v", resp.Members)
}

// Scenario: ?status=idle retires ONLY the idle members of a group — the
// "Retire idle agents in <group>" palette command. An online-but-working
// member and an offline member are both left untouched AND omitted from
// the response (the filter excludes them, it doesn't list them as
// skips). Pins the server-side status filter that backs the palette.
func TestGroupRetire_StatusFilterIdleOnly(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const idleConv = "idle-1111-2222-3333-4444"
	const workConv = "work-1111-2222-3333-4444"
	const offConv = "offl-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(idleConv, "idle-worker")
	f.HaveConvWithTitle(workConv, "busy-worker")
	f.HaveConvWithTitle(offConv, "offline-worker")
	// idle + working are online; offline has no live session at all.
	f.HaveAliveSession(idleConv, "spwn-idle", "tmux-idle", f.TestCwd("idle"))
	f.HaveAliveSession(workConv, "spwn-work", "tmux-work", f.TestCwd("work"))
	setConvStatus(t, idleConv, "idle")
	setConvStatus(t, workConv, "working")
	f.HaveMember(group, idleConv)
	f.HaveMember(group, workConv)
	f.HaveMember(group, offConv)

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "status=idle")
	require.Equal(t, http.StatusOK, code)

	// Only the idle member is retired and listed.
	assert.Equal(t, "retired", retireMemberAction(resp, idleConv), "members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, workConv),
		"a working member must be filtered out (omitted), not listed; members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, offConv),
		"an offline member must be filtered out of a status=idle retire; members=%+v", resp.Members)
	assert.Len(t, resp.Members, 1, "status=idle must list only the idle cohort; members=%+v", resp.Members)

	// The idle member is demoted + soft-exited.
	idleState, err := db.AgentState(idleConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, idleState, "the idle member must be retired")
	assert.False(t, f.World.Tmux.IsAlive("tmux-idle"), "the idle member's pane is soft-exited")

	// The working + offline members are completely untouched.
	for _, c := range []string{workConv, offConv} {
		state, err := db.AgentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.AgentStateActive, state, "%s must stay active under a status=idle retire", c)
		assert.True(t, flowGroupHasMember(f, group, c), "%s must stay a member", c)
	}
	assert.True(t, f.World.Tmux.IsAlive("tmux-work"), "a working member's pane must not be touched")
}

// Scenario: ?status=offline retires ONLY the members with no live
// session — the "Retire offline agents in <group>" palette command. An
// online (idle) member is left untouched and omitted.
func TestGroupRetire_StatusFilterOffline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const onlineConv = "onln-1111-2222-3333-4444"
	const offConv = "ofln-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(onlineConv, "online-worker")
	f.HaveConvWithTitle(offConv, "offline-worker")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", f.TestCwd("onln"))
	setConvStatus(t, onlineConv, "idle")
	f.HaveMember(group, onlineConv)
	f.HaveMember(group, offConv) // enrolled, but never had a session → offline

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "status=offline")
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, "retired", retireMemberAction(resp, offConv), "members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, onlineConv),
		"an online member must be filtered out of a status=offline retire; members=%+v", resp.Members)
	assert.Len(t, resp.Members, 1, "status=offline must list only the offline cohort; members=%+v", resp.Members)

	offState, err := db.AgentState(offConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, offState, "the offline member must be retired")

	onlineState, err := db.AgentState(onlineConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, onlineState, "the online member must stay active")
	assert.True(t, f.World.Tmux.IsAlive("tmux-onln"), "the online member's pane must not be touched")
}

// Scenario: the dashboard route POST /api/groups/{name}/retire?status=idle
// (the cookie-auth twin the command palette's "Retire idle agents in
// <group>" actually calls) retires the idle cohort, just like the /v1
// surface. Pins the dashboard wiring end to end (route → shared core),
// distinct from the SO_PEERCRED /v1 path the scenarios above drive.
func TestDashboardGroupRetire_StatusFilterIdle(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f // the dashboard mux shares the same process DB the flow seeded

	const group = "dash-team"
	const idleConv = "didl-1111-2222-3333-4444"
	const workConv = "dwrk-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(idleConv, "idle-worker")
	f.HaveConvWithTitle(workConv, "busy-worker")
	f.HaveAliveSession(idleConv, "spwn-didl", "tmux-didl", f.TestCwd("didl"))
	f.HaveAliveSession(workConv, "spwn-dwrk", "tmux-dwrk", f.TestCwd("dwrk"))
	setConvStatus(t, idleConv, "idle")
	setConvStatus(t, workConv, "working")
	f.HaveMember(group, idleConv)
	f.HaveMember(group, workConv)

	mux := agentd.BuildDashboardHandlerForTest()
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire?status=idle", nil)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "POST /api/groups/%s/retire body=%s", group, rec.Body.String())
	var resp groupRetireResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode: %s", rec.Body.String())

	assert.Equal(t, "retired", retireMemberAction(resp, idleConv), "members=%+v", resp.Members)
	assert.Len(t, resp.Members, 1, "the dashboard route must apply the idle filter; members=%+v", resp.Members)

	idleState, err := db.AgentState(idleConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, idleState, "the idle member must be retired via the dashboard route")
	workState, err := db.AgentState(workConv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, workState, "the working member must stay active")
}

// Scenario: the dashboard retire-preview path posts an EXPLICIT conv-id
// list (the rows the human ticked in the preview modal), and the BE
// retires precisely that set — never a cohort it re-derived from live
// status. Three idle/working members; the body selects two of them
// (omitting the third), and one of the two is "working", not idle. The
// outcome proves the two selected are retired REGARDLESS of their live
// status (no status re-filter) and the unselected third stays untouched
// even though it would match a status=idle sweep. This is the property
// that keeps "what the human previewed" == "what the BE retires".
func TestDashboardGroupRetire_ExplicitConvsSelection(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "dash-explicit"
	const pickIdle = "epia-1111-2222-3333-4444" // selected, idle
	const pickWork = "epwk-1111-2222-3333-4444" // selected, WORKING (status ignored)
	const keep = "epkp-1111-2222-3333-4444"     // NOT selected, idle → must survive
	f.HaveGroup(group)
	f.HaveConvWithTitle(pickIdle, "picked-idle")
	f.HaveConvWithTitle(pickWork, "picked-working")
	f.HaveConvWithTitle(keep, "kept-idle")
	f.HaveAliveSession(pickIdle, "spwn-epia", "tmux-epia", f.TestCwd("epia"))
	f.HaveAliveSession(pickWork, "spwn-epwk", "tmux-epwk", f.TestCwd("epwk"))
	f.HaveAliveSession(keep, "spwn-epkp", "tmux-epkp", f.TestCwd("epkp"))
	setConvStatus(t, pickIdle, "idle")
	setConvStatus(t, pickWork, "working")
	setConvStatus(t, keep, "idle")
	f.HaveMember(group, pickIdle)
	f.HaveMember(group, pickWork)
	f.HaveMember(group, keep)

	mux := agentd.BuildDashboardHandlerForTest()
	body := map[string]any{"convs": []string{pickIdle, pickWork}, "shutdown": true}
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire", body)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "POST /api/groups/%s/retire body=%s", group, rec.Body.String())
	var resp groupRetireResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode: %s", rec.Body.String())

	// Exactly the two selected are retired and listed; the working one too
	// — the explicit path never re-applies a status filter.
	assert.Equal(t, "retired", retireMemberAction(resp, pickIdle), "members=%+v", resp.Members)
	assert.Equal(t, "retired", retireMemberAction(resp, pickWork),
		"an explicitly-selected member is retired regardless of live status; members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, keep),
		"an unselected member must be omitted, never retired; members=%+v", resp.Members)
	assert.Len(t, resp.Members, 2, "only the two selected convs are acted on; members=%+v", resp.Members)

	for _, c := range []string{pickIdle, pickWork} {
		state, err := db.AgentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.AgentStateRetired, state, "%s must be retired", c)
	}
	// The unselected member is fully intact: active, still a member, still online.
	keepState, err := db.AgentState(keep)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, keepState, "the unselected member stays active")
	assert.True(t, flowGroupHasMember(f, group, keep), "the unselected member stays in the group")
	assert.True(t, f.World.Tmux.IsAlive("tmux-epkp"), "the unselected member's pane must not be touched")
	// shutdown:true soft-exits the two selected panes.
	assert.False(t, f.World.Tmux.IsAlive("tmux-epia"), "selected idle member's pane is soft-exited")
	assert.False(t, f.World.Tmux.IsAlive("tmux-epwk"), "selected working member's pane is soft-exited")
}

// Scenario: an explicit-convs retire with ?status= present ignores the
// query status entirely — the body's selection wins, so a member that
// would be filtered OUT by the status is still retired when ticked. Pins
// "explicit selection beats the status filter" so a refactor can't let a
// stray query param silently narrow the human's previewed list.
func TestDashboardGroupRetire_ExplicitConvsOverrideStatusQuery(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "dash-override"
	const conv = "eovr-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(conv, "working-but-picked")
	f.HaveAliveSession(conv, "spwn-eovr", "tmux-eovr", f.TestCwd("eovr"))
	setConvStatus(t, conv, "working") // would be excluded by status=idle
	f.HaveMember(group, conv)

	mux := agentd.BuildDashboardHandlerForTest()
	body := map[string]any{"convs": []string{conv}}
	// ?status=idle is present but must be ignored when convs is supplied.
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire?status=idle", body)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp groupRetireResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode: %s", rec.Body.String())

	assert.Equal(t, "retired", retireMemberAction(resp, conv),
		"the explicit selection wins over ?status=idle; members=%+v", resp.Members)
	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state, "the picked member must be retired")
}

// Scenario: a conv-id in the explicit `convs` list that is NOT a member
// of the group is silently ignored — the membership table is
// authoritative, the explicit set only NARROWS it. This pins the security
// invariant that the explicit path can never retire a conv outside the
// group, no matter what the body asks for. A real active agent that is not
// a member stays fully untouched.
func TestDashboardGroupRetire_ExplicitConvsIgnoresNonMember(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "dash-nonmember"
	const member = "nmem-1111-2222-3333-4444"   // in the group, selected
	const outsider = "nout-1111-2222-3333-4444" // active agent, NOT in the group
	f.HaveGroup(group)
	f.HaveGroup("other-team")
	f.HaveConvWithTitle(member, "member")
	f.HaveConvWithTitle(outsider, "outsider")
	f.HaveAliveSession(member, "spwn-nmem", "tmux-nmem", f.TestCwd("nmem"))
	f.HaveAliveSession(outsider, "spwn-nout", "tmux-nout", f.TestCwd("nout"))
	f.HaveMember(group, member)
	f.HaveMember("other-team", outsider) // outsider is an agent, but of another group

	mux := agentd.BuildDashboardHandlerForTest()
	// The body asks to retire BOTH, but the outsider is not a member here.
	body := map[string]any{"convs": []string{member, outsider}}
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire", body)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp groupRetireResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode: %s", rec.Body.String())

	assert.Equal(t, "retired", retireMemberAction(resp, member), "members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, outsider),
		"a non-member conv-id must never be acted on, even when the body lists it; members=%+v", resp.Members)
	assert.Len(t, resp.Members, 1, "only the real member is acted on; members=%+v", resp.Members)

	// The outsider is fully intact: still an active agent, still online.
	state, err := db.AgentState(outsider)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "a non-member must stay an active agent")
	assert.True(t, f.World.Tmux.IsAlive("tmux-nout"), "a non-member's pane must not be touched")
}

// Scenario: an explicit but EMPTY convs list ({"convs":[]}) is a 400, NOT
// a silent fallthrough to "retire every member". Pins the footgun guard:
// a present convs key always selects the explicit path, and an empty one
// is a client error rather than a whole-group sweep.
func TestDashboardGroupRetire_EmptyConvsRejected(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "dash-emptyconvs"
	const conv = "ecnv-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(conv, "should-survive")
	f.HaveAliveSession(conv, "spwn-ecnv", "tmux-ecnv", f.TestCwd("ecnv"))
	f.HaveMember(group, conv)

	mux := agentd.BuildDashboardHandlerForTest()
	body := map[string]any{"convs": []string{}}
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire", body)
	rec := testharness.Serve(mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"an explicit empty convs list must be a 400, never a retire-everyone; body=%s", rec.Body.String())

	// The member is untouched: a rejected request must demote nobody.
	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "an empty-convs 400 must not demote anyone")
	assert.True(t, f.World.Tmux.IsAlive("tmux-ecnv"), "an empty-convs 400 must not stop sessions")
}

// Scenario: an unknown ?status= token is rejected with 400, not silently
// treated as "match nobody" (which would 200 with an empty member list,
// indistinguishable from "the group has no agents of that status"). The
// group is left completely untouched.
func TestGroupRetire_UnknownStatusRejected(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "tclaude-dev"
	const conv = "ukst-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(conv, "worker")
	f.HaveAliveSession(conv, "spwn-ukst", "tmux-ukst", f.TestCwd("ukst"))
	f.HaveMember(group, conv)

	code, _ := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "status=offlien")
	assert.Equal(t, http.StatusBadRequest, code,
		"an unknown status token must be 400, not a silent no-op")

	// The member is untouched: still an active agent, still online.
	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "a rejected retire must not demote anyone")
	assert.True(t, f.World.Tmux.IsAlive("tmux-ukst"), "a rejected retire must not stop sessions")
}

// Scenario: a bulk retire that demotes two members who each SOLELY own a
// DIFFERENT other group surfaces an ownerless warning for BOTH groups —
// proving the parallel ownerless-merge gathers the owner-groups every
// worker touched, not just whichever finished last. Exercises the
// post-Wait aggregation under real concurrency (CI runs it with -race).
func TestGroupRetire_ParallelOwnerlessMergeAcrossMembers(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const team = "tclaude-dev"
	const ownerA = "owna-1111-2222-3333-4444"
	const ownerB = "ownb-1111-2222-3333-4444"
	const plain = "plan-1111-2222-3333-4444"
	f.HaveGroup(team)
	alpha := f.HaveGroup("alpha")
	beta := f.HaveGroup("beta")
	for _, c := range []string{ownerA, ownerB, plain} {
		f.HaveConvWithTitle(c, c[:4])
		f.HaveMember(team, c)
	}
	// ownerA solely owns alpha; ownerB solely owns beta — and each is a
	// member of the group it owns (retire only unjoins groups the conv is
	// a member of). Retiring them from `team` cascades a full demotion
	// that unjoins them everywhere, emptying alpha's + beta's owner
	// rosters — so each worker reports a DIFFERENT ownerless group.
	f.HaveMember("alpha", ownerA)
	f.HaveMember("beta", ownerB)
	require.NoError(t, db.AddAgentGroupOwner(alpha.ID, ownerA, "human"))
	require.NoError(t, db.AddAgentGroupOwner(beta.ID, ownerB, "human"))

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, team, "")
	require.Equal(t, http.StatusOK, code)
	for _, c := range []string{ownerA, ownerB, plain} {
		assert.Equal(t, "retired", retireMemberAction(resp, c), "members=%+v", resp.Members)
	}

	// Both now-ownerless groups must be warned about — the merge gathered
	// owner-groups from BOTH workers, not just one.
	joined := strings.Join(resp.Warnings, "\n")
	assert.Contains(t, joined, `"alpha"`, "alpha lost its only owner; warnings=%v", resp.Warnings)
	assert.Contains(t, joined, `"beta"`, "beta lost its only owner; warnings=%v", resp.Warnings)
}

// Scenario (JOH-327): the dashboard's retire preview now submits stable
// agent_ids (the conv_id phase-out), and the matcher must resolve each
// selector back to the member's conv-id. This pins that an explicit
// `convs` list of agt_ ids retires exactly those members — AND that the
// raw conv-id path STILL works in the same request (the conv-keyed
// back-compat path D2 deliberately preserved): one member is selected by
// its agt_ id, another by its raw conv-id, both retire, and an
// unselected third survives.
//
// The raw-conv-id acceptance is what underpins the dangling-agent
// recovery escape hatch (PR #628) — a member must stay retirable by a
// conv-id even when the dashboard couldn't lead with an agent_id. (The
// deeper looksLikeConvID UUID-shape fallback inside resolveCleanupConv
// is belt-and-suspenders that a group member never reaches: membership
// alone makes a conv resolvable via ResolveSelector's group-member
// branch, so this test exercises that resolution path, not the
// shape-only fallback.)
func TestDashboardGroupRetire_AcceptsAgentIDAndConvIDSelectors(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "dash-agentid"
	const byAgentID = "agid-1111-2222-3333-4444" // selected via its agt_ id
	const byConvID = "cvid-1111-2222-3333-4444"  // selected via its raw conv-id
	const keep = "keep-1111-2222-3333-4444"      // not selected → must survive
	f.HaveGroup(group)
	for _, c := range []string{byAgentID, byConvID, keep} {
		f.HaveConvWithTitle(c, "w-"+c[:4])
		f.HaveAliveSession(c, "spwn-"+c[:4], "tmux-"+c[:4], f.TestCwd(c[:4]))
		f.HaveMember(group, c)
	}

	// The stable agent_id of the first member — what the flipped picker emits.
	agentID, err := db.AgentIDForConv(byAgentID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "member must have a stable agent_id")
	require.Contains(t, agentID, "agt_", "the selector must be an agt_ id, not a conv-id")

	mux := agentd.BuildDashboardHandlerForTest()
	body := map[string]any{"convs": []string{agentID, byConvID}, "shutdown": true}
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire", body)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp groupRetireResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode: %s", rec.Body.String())

	// Both selected members retire — one resolved from its agt_ id, one
	// from its raw conv-id — and the response keys back on conv-id.
	assert.Equal(t, "retired", retireMemberAction(resp, byAgentID),
		"a member selected by its agt_ id must retire; members=%+v", resp.Members)
	assert.Equal(t, "retired", retireMemberAction(resp, byConvID),
		"a member selected by its raw conv-id must still retire (conv-keyed back-compat); members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, keep),
		"an unselected member must be omitted; members=%+v", resp.Members)
	assert.Len(t, resp.Members, 2, "only the two selected members are acted on; members=%+v", resp.Members)

	for _, c := range []string{byAgentID, byConvID} {
		state, serr := db.AgentState(c)
		require.NoError(t, serr)
		assert.Equal(t, db.AgentStateRetired, state, "%s must be retired", c)
	}
	keepState, err := db.AgentState(keep)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, keepState, "the unselected member stays active")
	assert.True(t, flowGroupHasMember(f, group, keep), "the unselected member stays in the group")
	assert.True(t, f.World.Tmux.IsAlive("tmux-keep"), "the unselected member's pane must not be touched")
}
