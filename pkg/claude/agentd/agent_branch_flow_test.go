package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: two agents in a group, each running in its own git
// worktree on a different branch. Their branch must surface on every
// listing the human and the dashboard read from:
//
//   - GET /v1/groups/{name}/members — `tclaude agent groups members`
//   - GET /v1/peers                 — `tclaude agent ls`
//   - GET /api/snapshot             — dashboard groups + agents tabs
//
// Claude Code stamps `gitBranch` into every .jsonl turn; the CCSim
// mirrors that via HaveAliveSessionOnBranch, so a conv_index scan
// resolves the branch through the production read path. This pins the
// wiring: a renamed/moved column or a dropped struct field on any of
// the four surfaces fails here.
func TestAgentBranch_SurfacedAcrossListings(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const aliceConv = "aaaaaaaa-1111-2222-3333-444444444444"
	const bobConv = "bbbbbbbb-1111-2222-3333-444444444444"
	wantBranch := map[string]string{
		aliceConv: "feature-login",
		bobConv:   "bugfix-crash",
	}

	f.HaveGroup("squad")
	f.HaveAliveSessionOnBranch(aliceConv, "spwn-alice", "tmux-alice", "/tmp/wt/login", wantBranch[aliceConv])
	f.HaveAliveSessionOnBranch(bobConv, "spwn-bob", "tmux-bob", "/tmp/wt/crash", wantBranch[bobConv])
	f.HaveMember("squad", aliceConv, "alice")
	f.HaveMember("squad", bobConv, "bob")

	// Stand in for the watch model: scan each conv's .jsonl into
	// conv_index so the cached-read surfaces (peers, group members)
	// resolve the branch. The dashboard refreshes conv_index itself.
	require.NotNil(t, agent.FreshConvRowResolved(aliceConv), "alice conv_index scan")
	require.NotNil(t, agent.FreshConvRowResolved(bobConv), "bob conv_index scan")

	// Surface 1: GET /v1/groups/squad/members.
	membersSeen := map[string]string{}
	for _, m := range f.ListGroupMembers("squad") {
		membersSeen[m.ConvID] = m.Branch
	}
	assert.Equal(t, wantBranch[aliceConv], membersSeen[aliceConv], "groups members branch for alice")
	assert.Equal(t, wantBranch[bobConv], membersSeen[bobConv], "groups members branch for bob")

	// Surface 2: GET /v1/peers.
	peersReq := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/peers", nil))
	peersRec := testharness.Serve(f.Mux, peersReq)
	require.Equal(t, http.StatusOK, peersRec.Code, "/v1/peers body=%s", peersRec.Body.String())
	var peers []struct {
		ConvID string `json:"conv_id"`
		Branch string `json:"branch"`
	}
	require.NoError(t, json.Unmarshal(peersRec.Body.Bytes(), &peers), "decode /v1/peers")
	peersSeen := map[string]string{}
	for _, p := range peers {
		peersSeen[p.ConvID] = p.Branch
	}
	assert.Equal(t, wantBranch[aliceConv], peersSeen[aliceConv], "peers branch for alice")
	assert.Equal(t, wantBranch[bobConv], peersSeen[bobConv], "peers branch for bob")

	// Surface 3: GET /api/snapshot — dashboard agents + groups tabs.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	agentsSeen := map[string]string{}
	for _, a := range snap.Agents {
		agentsSeen[a.ConvID] = a.Branch
	}
	assert.Equal(t, wantBranch[aliceConv], agentsSeen[aliceConv], "dashboard agents-tab branch for alice")
	assert.Equal(t, wantBranch[bobConv], agentsSeen[bobConv], "dashboard agents-tab branch for bob")

	var squad *dashGroup
	for i := range snap.Groups {
		if snap.Groups[i].Name == "squad" {
			squad = &snap.Groups[i]
		}
	}
	require.NotNil(t, squad, "dashboard snapshot missing group squad")
	groupMembersSeen := map[string]string{}
	for _, m := range squad.Members {
		groupMembersSeen[m.ConvID] = m.Branch
	}
	assert.Equal(t, wantBranch[aliceConv], groupMembersSeen[aliceConv], "dashboard groups-tab branch for alice")
	assert.Equal(t, wantBranch[bobConv], groupMembersSeen[bobConv], "dashboard groups-tab branch for bob")
}

// Scenario: an agent starts a session on `main`, then runs
// `git checkout -b feature-x` partway through. Claude Code stamps the
// *current* branch onto every .jsonl turn, so the conv_index scan
// reports two branches: the last-wins `git_branch` (where the agent
// is now) and the first-wins `git_branch_startup` (the immutable
// launch branch). The dashboard renders them as a "now / init" pair.
//
// This stands an agent up on `main`, writes a later turn on
// `feature-x` (as a real branch switch would), rescans, and asserts
// that `branch` follows the switch while `startup_branch` does not.
func TestAgentBranch_LastWinsAfterMidSessionSwitch(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "cccccccc-1111-2222-3333-444444444444"

	f.HaveGroup("squad")
	f.HaveAliveSessionOnBranch(conv, "spwn-x", "tmux-x", "/tmp/wt/x", "main")
	f.HaveMember("squad", conv, "switcher")

	// First scan: the agent is still on the branch it started on, so
	// the current and startup branches agree.
	row := agent.FreshConvRowResolved(conv)
	require.NotNil(t, row, "initial conv_index scan")
	require.Equal(t, "main", row.GitBranch, "agent should start on main")
	require.Equal(t, "main", row.GitBranchStartup, "startup branch should be main")

	// The agent runs `git checkout -b feature-x` mid-conversation; CC
	// stamps the new branch onto the next turn it writes to the .jsonl.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "CCSim for conv")
	cc.GitBranch = "feature-x"
	require.NoError(t, cc.WriteUserTurn("after git checkout -b feature-x"), "write branch-switch turn")

	// FreshBranch refreshes conv_index from the .jsonl — the file grew,
	// so the mtime/size freshness check forces a rescan.
	assert.Equal(t, "feature-x", agent.FreshBranch(conv), "FreshBranch after switch")

	// The rescan moved git_branch forward but git_branch_startup — the
	// branch the first turn was stamped with — must stay put.
	row = agent.FreshConvRowResolved(conv)
	require.NotNil(t, row, "conv_index rescan after switch")
	assert.Equal(t, "feature-x", row.GitBranch, "git_branch follows the switch")
	assert.Equal(t, "main", row.GitBranchStartup, "git_branch_startup is the immutable launch branch")

	// Surface 1: GET /v1/groups/squad/members.
	membersSeen := map[string]string{}
	for _, m := range f.ListGroupMembers("squad") {
		membersSeen[m.ConvID] = m.Branch
	}
	assert.Equal(t, "feature-x", membersSeen[conv], "groups members branch after switch")

	// Surface 2: GET /api/snapshot — dashboard groups tab. The member
	// row carries both branches: `branch` (now) follows the switch,
	// `startup_branch` (init) stays the launch branch.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	var squad *dashGroup
	for i := range snap.Groups {
		if snap.Groups[i].Name == "squad" {
			squad = &snap.Groups[i]
		}
	}
	require.NotNil(t, squad, "dashboard snapshot missing group squad")
	var member *dashMember
	for i := range squad.Members {
		if squad.Members[i].ConvID == conv {
			member = &squad.Members[i]
		}
	}
	require.NotNil(t, member, "conv missing from squad members")
	assert.Equal(t, "feature-x", member.Branch,
		"dashboard groups-tab `branch` tracks the current branch after the switch")
	assert.Equal(t, "main", member.StartupBranch,
		"dashboard groups-tab `startup_branch` stays the launch branch after a mid-session checkout")
}
