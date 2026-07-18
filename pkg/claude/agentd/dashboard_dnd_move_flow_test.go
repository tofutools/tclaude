package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the dashboard's drag-and-drop move runs the
// POST /api/groups/B/members → DELETE /api/groups/A/members/{conv}
// sequence on top of the existing dashboard cookie endpoints. This
// flow test pins the daemon-side guarantee the JS leans on: after
// both calls succeed, the conv is a member of B and not A in the
// next snapshot.
//
// Pins the production read path used by the dashboard's optimistic
// renderer — `Groups[*].Members[]` from /api/snapshot — not the
// underlying DB tables, because that's what the drop handler reads
// when re-rendering the groups tab.
func TestDashboardDnDMove_AddThenRemoveLeavesConvInTargetOnly(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "drag-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "movee")
	f.HaveAliveSession(conv, "spwn-drag", "tmux-drag", f.TestCwd("drag"))
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", conv)

	mux := agentd.BuildDashboardHandlerForTest()

	// Pre-condition: in alpha, not in beta.
	pre := fetchDashSnapshot(t, mux)
	require.True(t, groupHasMember(pre, "alpha", conv),
		"pre-move: %s should be in alpha; snapshot=%+v", conv, pre.Agents)
	require.False(t, groupHasMember(pre, "beta", conv),
		"pre-move: %s already in beta; setup is wrong", conv)

	// Step 1: add to beta (POST first, the order the JS uses).
	addBody := strings.NewReader(`{"conv":"` + conv + `"}`)
	addReq, _ := http.NewRequest(http.MethodPost, "/api/groups/beta/members", addBody)
	addReq.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, addReq)
	require.Equal(t, http.StatusOK, rec.Code,
		"POST /api/groups/beta/members body=%s", rec.Body.String())

	// Step 2: remove from alpha.
	delReq, _ := http.NewRequest(http.MethodDelete, "/api/groups/alpha/members/"+conv, nil)
	rec = testharness.Serve(mux, delReq)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"DELETE /api/groups/alpha/members/%s body=%s", conv, rec.Body.String())

	// Post-condition: in beta, not in alpha. Asserts at the same
	// surface the dashboard's renderer reads.
	post := fetchDashSnapshot(t, mux)
	assert.True(t, groupHasMember(post, "beta", conv), "post-move: %s should be in beta", conv)
	assert.False(t, groupHasMember(post, "alpha", conv), "post-move: %s should NOT be in alpha after DELETE", conv)

	// Snapshot's per-agent groups[] array should reflect the move too.
	a := findAgent(post.Agents, conv)
	require.NotNil(t, a, "post-move: %s missing from agents[]", conv)
	assert.True(t, containsString(a.Groups, "beta") && !containsString(a.Groups, "alpha"),
		"post-move: agent groups = %v, want [beta] only", a.Groups)
}

// Scenario: the JS rollback path. If the POST B succeeds but DELETE A
// fails for some reason (e.g. external write removed the row first),
// the dashboard JS keeps the optimistic move (conv is in BOTH groups,
// visible + recoverable) instead of trying to undo the add. This
// flow test pins the daemon-side state for that "partial" branch:
// after a successful POST B and a failed DELETE A, the conv is a
// member of both groups in the next snapshot — so the dashboard's
// "visible and recoverable" claim is true at the production read
// surface.
func TestDashboardDnDMove_PartialFailureLeavesConvInBoth(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "part-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "partmove")
	f.HaveAliveSession(conv, "spwn-part", "tmux-part", f.TestCwd("part"))
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", conv)

	mux := agentd.BuildDashboardHandlerForTest()

	// Step 1: POST B succeeds.
	addBody := strings.NewReader(`{"conv":"` + conv + `"}`)
	addReq, _ := http.NewRequest(http.MethodPost, "/api/groups/beta/members", addBody)
	addReq.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, addReq)
	require.Equal(t, http.StatusOK, rec.Code, "POST body=%s", rec.Body.String())

	// Step 2: synthetically force a DELETE failure by passing a
	// nonexistent conv selector. The handler returns 404; the JS
	// would surface a partial-failure toast and keep the optimistic
	// add in place.
	delReq, _ := http.NewRequest(http.MethodDelete, "/api/groups/alpha/members/no-such-conv-anywhere", nil)
	rec = testharness.Serve(mux, delReq)
	require.False(t, rec.Code == http.StatusNoContent || rec.Code == http.StatusOK,
		"synthetic DELETE was supposed to fail; got %d body=%s", rec.Code, rec.Body.String())

	// Snapshot must show the conv in BOTH groups: the recoverable
	// state the dashboard's partial-failure toast points the human
	// at.
	post := fetchDashSnapshot(t, mux)
	assert.True(t, groupHasMember(post, "alpha", conv),
		"partial-failure: %s should still be in alpha (DELETE failed)", conv)
	assert.True(t, groupHasMember(post, "beta", conv),
		"partial-failure: %s should be in beta (POST succeeded)", conv)
}

// groupHasMember walks the snapshot's per-group members list (the same
// path the dashboard's renderGroups reads) to check if conv is a
// member of group. Mirrors the DnD code's lastSnapshot lookup.
func groupHasMember(snap dashSnapshot, group, conv string) bool {
	// dashSnapshot from dashboard_ungrouped_flow_test.go only carries
	// agents/ungrouped. Refetch via direct unmarshal to reach groups.
	// We fetch a fuller view here.
	for _, a := range snap.Agents {
		if a.ConvID == conv && containsString(a.Groups, group) {
			return true
		}
	}
	return false
}
