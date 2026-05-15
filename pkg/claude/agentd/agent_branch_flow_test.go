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
