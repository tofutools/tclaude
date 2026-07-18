package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// ungroupedHas reports whether conv appears in the snapshot's
// ungrouped[] array — the production read surface the dashboard's
// virtual "Ungrouped" group renders from.
func ungroupedHas(snap dashSnapshot, conv string) bool {
	for _, a := range snap.Ungrouped {
		if a.ConvID == conv {
			return true
		}
	}
	return false
}

// Scenario: the dashboard's virtual "Ungrouped" group is a drag
// SOURCE. Dragging one of its rows onto a real group's header runs
// `runDndAddToGroup`, which fires a single POST
// /api/groups/{B}/members — the agent had no group, so there's
// nothing to remove.
//
// This flow test pins the daemon-side guarantee that gesture leans
// on: after the POST, the conv is a member of the target group AND
// has dropped out of the snapshot's ungrouped[] array, so it leaves
// the virtual group on the dashboard's next render.
func TestDashboardUngrouped_DragIntoGroupAddsAndLeavesUngrouped(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "ungr-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "loose-worker")
	f.HaveAliveSession(conv, "spwn-ungr", "tmux-ungr", f.TestCwd("ungr"))
	f.HaveEnrolledAgent(conv) // an enrolled, ungrouped, online agent
	f.HaveGroup("alpha")

	mux := agentd.BuildDashboardHandlerForTest()

	// Pre-condition: online, in no group → surfaced in ungrouped[].
	pre := fetchDashSnapshot(t, mux)
	require.True(t, ungroupedHas(pre, conv),
		"pre-drag: %s should be in ungrouped[]; got %d rows", conv, len(pre.Ungrouped))
	require.False(t, groupHasMember(pre, "alpha", conv),
		"pre-drag: %s already in alpha; setup is wrong", conv)

	// The drag: runDndAddToGroup → POST /api/groups/alpha/members.
	addBody := strings.NewReader(`{"conv":"` + conv + `"}`)
	addReq, _ := http.NewRequest(http.MethodPost, "/api/groups/alpha/members", addBody)
	addReq.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, addReq)
	require.Equal(t, http.StatusOK, rec.Code,
		"POST /api/groups/alpha/members body=%s", rec.Body.String())

	// Post-condition: in alpha, gone from ungrouped[].
	post := fetchDashSnapshot(t, mux)
	assert.True(t, groupHasMember(post, "alpha", conv),
		"post-drag: %s should be a member of alpha", conv)
	assert.False(t, ungroupedHas(post, conv),
		"post-drag: %s should have left ungrouped[] (it's in alpha now)", conv)
}

// Scenario: the virtual "Ungrouped" group is also a drag TARGET.
// Dragging a real group's member row onto the Ungrouped header runs
// `runDndRemoveFromGroup`, which fires a single DELETE
// /api/groups/{A}/members/{conv}.
//
// This flow test pins the daemon-side guarantee: when the dropped
// group was the agent's ONLY group, after the DELETE the conv is no
// longer a member of it AND reappears in the snapshot's ungrouped[]
// array — so it lands back in the virtual group on the dashboard.
func TestDashboardUngrouped_DragOutOfGroupReturnsToUngrouped(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "drop-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "grouped-worker")
	f.HaveAliveSession(conv, "spwn-drop", "tmux-drop", f.TestCwd("drop"))
	f.HaveGroup("alpha")
	f.HaveMember("alpha", conv)

	mux := agentd.BuildDashboardHandlerForTest()

	// Pre-condition: member of alpha → NOT in ungrouped[].
	pre := fetchDashSnapshot(t, mux)
	require.True(t, groupHasMember(pre, "alpha", conv),
		"pre-drag: %s should be in alpha; setup is wrong", conv)
	require.False(t, ungroupedHas(pre, conv),
		"pre-drag: %s should not be in ungrouped[] while it's in alpha", conv)

	// The drag onto Ungrouped: runDndRemoveFromGroup →
	// DELETE /api/groups/alpha/members/{conv}.
	delReq, _ := http.NewRequest(http.MethodDelete, "/api/groups/alpha/members/"+conv, nil)
	rec := testharness.Serve(mux, delReq)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"DELETE /api/groups/alpha/members/%s body=%s", conv, rec.Body.String())

	// Post-condition: gone from alpha, back in ungrouped[].
	post := fetchDashSnapshot(t, mux)
	assert.False(t, groupHasMember(post, "alpha", conv),
		"post-drag: %s should no longer be a member of alpha", conv)
	assert.True(t, ungroupedHas(post, conv),
		"post-drag: %s should be back in ungrouped[] (alpha was its only group)", conv)
}

// Scenario: one of the virtual Ungrouped group's stated purposes is
// "finding agents for deleted groups". When a group is deleted, its
// still-online members lose their last membership and must resurface
// somewhere the human can re-home them.
//
// This flow test pins exactly that: an online member of a group that
// is then deleted reappears in the snapshot's ungrouped[] array — so
// it shows up in the dashboard's virtual Ungrouped group rather than
// vanishing.
func TestDashboardUngrouped_DeletedGroupMembersSurfaceInUngrouped(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "orph-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(conv, "orphan-to-be")
	f.HaveAliveSession(conv, "spwn-orph", "tmux-orph", f.TestCwd("orph"))
	f.HaveGroup("doomed")
	f.HaveMember("doomed", conv)

	mux := agentd.BuildDashboardHandlerForTest()

	// Pre-condition: member of "doomed" → not loose.
	pre := fetchDashSnapshot(t, mux)
	require.True(t, groupHasMember(pre, "doomed", conv),
		"pre-delete: %s should be in doomed; setup is wrong", conv)
	require.False(t, ungroupedHas(pre, conv),
		"pre-delete: %s should not be ungrouped while doomed exists", conv)

	// Delete the whole group.
	delReq, _ := http.NewRequest(http.MethodDelete, "/api/groups/doomed", nil)
	rec := testharness.Serve(mux, delReq)
	require.True(t, rec.Code >= 200 && rec.Code < 300,
		"DELETE /api/groups/doomed: want 2xx, got %d body=%s", rec.Code, rec.Body.String())

	// Post-condition: the orphaned-but-online conv lands in ungrouped[].
	post := fetchDashSnapshot(t, mux)
	assert.True(t, ungroupedHas(post, conv),
		"post-delete: orphaned conv %s should surface in ungrouped[]", conv)
}

// Scenario: a permission grant-holder in no group is an ungrouped
// agent — holding a grant enrolls the conv, so it belongs in
// ungrouped[] whether or not its tmux pane is currently alive. The
// virtual "Ungrouped" group is where the human re-homes such a conv,
// so it must be visible there regardless of online state.
//
// Pins that handleDashboardSnapshot does NOT online-gate ungrouped[]:
// both an offline and an online grant-holder show up.
func TestDashboardSnapshot_UngroupedIncludesGrantHolders(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const offlineConv = "goff-1111-2222-3333-4444"
	const onlineConv = "gonl-1111-2222-3333-4444"
	f.HaveConvWithTitle(offlineConv, "offline-grant-holder")
	f.HaveConvWithTitle(onlineConv, "online-grant-holder")
	f.HaveAliveSession(offlineConv, "spwn-goff", "tmux-goff", f.TestCwd("goff"))
	f.HaveAliveSession(onlineConv, "spwn-gonl", "tmux-gonl", f.TestCwd("gonl"))
	f.MarkOffline("tmux-goff")

	// Both hold a permission grant but belong to no group — so the
	// daemon's per-conv perms loop pulls them into agentRows.
	require.NoError(t, db.GrantAgentPermission(offlineConv, "agent.send", "test"))
	require.NoError(t, db.GrantAgentPermission(onlineConv, "agent.send", "test"))

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	assert.True(t, ungroupedHas(snap, offlineConv),
		"offline grant-holder %s should appear in ungrouped[]", offlineConv)
	assert.True(t, ungroupedHas(snap, onlineConv),
		"online grant-holder %s should appear in ungrouped[]; got %d rows",
		onlineConv, len(snap.Ungrouped))
}

// Scenario: the virtual "Ungrouped" group is a dashboard-render
// concept with NO backing DB row. Its display name ("Ungrouped")
// must carry no backend authority — group-mutation endpoints aimed at
// that name fail with a not-found error exactly like any other
// unknown group, so the virtual group genuinely cannot be renamed,
// deleted or otherwise operated on.
func TestDashboardUngrouped_VirtualNameHasNoBackendGroup(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	// Deleting "Ungrouped" — there is no such real group, so the
	// daemon refuses it (no 2xx). The dashboard never offers a delete
	// button for the virtual group; this pins that even a hand-rolled
	// request can't destroy it.
	delReq, _ := http.NewRequest(http.MethodDelete, "/api/groups/Ungrouped", nil)
	rec := testharness.Serve(mux, delReq)
	assert.False(t, rec.Code >= 200 && rec.Code < 300,
		"DELETE /api/groups/Ungrouped should fail (no such real group); got %d body=%s",
		rec.Code, rec.Body.String())

	// Renaming "Ungrouped" likewise has nothing to act on.
	renBody := strings.NewReader(`{"new_name":"renamed"}`)
	renReq, _ := http.NewRequest(http.MethodPost, "/api/groups/Ungrouped/rename", renBody)
	renReq.Header.Set("Content-Type", "application/json")
	rec = testharness.Serve(mux, renReq)
	assert.False(t, rec.Code >= 200 && rec.Code < 300,
		"POST /api/groups/Ungrouped/rename should fail (no such real group); got %d body=%s",
		rec.Code, rec.Body.String())
}
