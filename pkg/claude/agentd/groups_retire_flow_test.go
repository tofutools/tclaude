package agentd_test

import (
	"encoding/json"
	"net/http"
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
