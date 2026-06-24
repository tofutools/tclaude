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
	f.HaveAliveSession(convA, "spwn-graa", "tmux-graa", "/tmp/graa")
	f.HaveAliveSession(convB, "spwn-grbb", "tmux-grbb", "/tmp/grbb")
	f.HaveMember(group, convA) // HaveMember enrolls
	f.HaveMember(group, convB)
	require.NoError(t, db.GrantAgentPermission(convA, "self.rename", "human"))

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "retired", retireMemberAction(resp, convA), "members=%+v", resp.Members)
	assert.Equal(t, "retired", retireMemberAction(resp, convB), "members=%+v", resp.Members)

	for _, c := range []string{convA, convB} {
		state, err := db.EnrollmentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.EnrollmentRetired, state, "%s must be retired", c)
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
	f.HaveAliveSession(worker, "spwn-nswk", "tmux-nswk", "/tmp/nswk")
	f.HaveMember(group, worker)
	// caller is an agent, but holds no groups.retire grant.
	f.HaveConvWithTitle(caller, "ungranted-coordinator")

	wrap := func(r *http.Request) *http.Request { return agentd.AsAgentPeer(r, caller) }
	code, _ := postGroupRetire(t, f.Mux, wrap, group, "")
	require.Equal(t, http.StatusForbidden, code, "an agent without groups.retire must be refused")

	// The worker is untouched: still an active agent, still a member,
	// still online.
	state, err := db.EnrollmentState(worker)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, state, "a refused retire must not demote anyone")
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
	f.HaveAliveSession(caller, "spwn-sfcl", "tmux-sfcl", "/tmp/sfcl")
	f.HaveAliveSession(workerA, "spwn-sfwa", "tmux-sfwa", "/tmp/sfwa")
	f.HaveAliveSession(workerB, "spwn-sfwb", "tmux-sfwb", "/tmp/sfwb")
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
	callerState, err := db.EnrollmentState(caller)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, callerState, "the caller stays an active agent")
	assert.True(t, flowGroupHasMember(f, group, caller), "the caller stays a member")
	assert.True(t, f.World.Tmux.IsAlive("tmux-sfcl"), "the caller's own pane is never /exit'd")

	// The workers are retired and stopped.
	for _, c := range []string{workerA, workerB} {
		state, err := db.EnrollmentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.EnrollmentRetired, state, "%s must be retired", c)
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
	f.HaveAliveSession(conv, "spwn-nsdn", "tmux-nsdn", "/tmp/nsdn")
	f.HaveMember(group, conv)

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "shutdown=0")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "retired", retireMemberAction(resp, conv), "members=%+v", resp.Members)

	state, err := db.EnrollmentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentRetired, state, "the member is still demoted")
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
	f.HaveAliveSession(idleConv, "spwn-idle", "tmux-idle", "/tmp/idle")
	f.HaveAliveSession(workConv, "spwn-work", "tmux-work", "/tmp/work")
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
	idleState, err := db.EnrollmentState(idleConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentRetired, idleState, "the idle member must be retired")
	assert.False(t, f.World.Tmux.IsAlive("tmux-idle"), "the idle member's pane is soft-exited")

	// The working + offline members are completely untouched.
	for _, c := range []string{workConv, offConv} {
		state, err := db.EnrollmentState(c)
		require.NoError(t, err)
		assert.Equal(t, db.EnrollmentActive, state, "%s must stay active under a status=idle retire", c)
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
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	setConvStatus(t, onlineConv, "idle")
	f.HaveMember(group, onlineConv)
	f.HaveMember(group, offConv) // enrolled, but never had a session → offline

	code, resp := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "status=offline")
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, "retired", retireMemberAction(resp, offConv), "members=%+v", resp.Members)
	assert.Equal(t, "", retireMemberAction(resp, onlineConv),
		"an online member must be filtered out of a status=offline retire; members=%+v", resp.Members)
	assert.Len(t, resp.Members, 1, "status=offline must list only the offline cohort; members=%+v", resp.Members)

	offState, err := db.EnrollmentState(offConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentRetired, offState, "the offline member must be retired")

	onlineState, err := db.EnrollmentState(onlineConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, onlineState, "the online member must stay active")
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
	f.HaveAliveSession(idleConv, "spwn-didl", "tmux-didl", "/tmp/didl")
	f.HaveAliveSession(workConv, "spwn-dwrk", "tmux-dwrk", "/tmp/dwrk")
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

	idleState, err := db.EnrollmentState(idleConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentRetired, idleState, "the idle member must be retired via the dashboard route")
	workState, err := db.EnrollmentState(workConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, workState, "the working member must stay active")
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
	f.HaveAliveSession(conv, "spwn-ukst", "tmux-ukst", "/tmp/ukst")
	f.HaveMember(group, conv)

	code, _ := postGroupRetire(t, f.Mux, agentd.AsHumanPeer, group, "status=offlien")
	assert.Equal(t, http.StatusBadRequest, code,
		"an unknown status token must be 400, not a silent no-op")

	// The member is untouched: still an active agent, still online.
	state, err := db.EnrollmentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, state, "a rejected retire must not demote anyone")
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
