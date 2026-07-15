package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// cleanupResp mirrors the unexported agentd.cleanupResponse so flow
// tests can decode the /api/cleanup/* result without importing the
// internal type.
type cleanupResp struct {
	Mode     string `json:"mode"`
	Outcomes []struct {
		ConvID string   `json:"conv_id"`
		Title  string   `json:"title"`
		Result string   `json:"result"`
		Detail string   `json:"detail"`
		Groups []string `json:"groups"`
	} `json:"outcomes"`
	Removed    int      `json:"removed"`
	Retired    int      `json:"retired"`
	Deleted    int      `json:"deleted"`
	Reinstated int      `json:"reinstated"`
	Skipped    int      `json:"skipped"`
	Failed     int      `json:"failed"`
	Warnings   []string `json:"warnings"`
}

// postCleanup fires a cleanup request at the dashboard mux and decodes
// the 200 response. Fatals on any non-200 — error-surface scenarios
// use a raw testharness.Serve instead.
func postCleanup(t *testing.T, mux http.Handler, path, body string) cleanupResp {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
	require.NoError(t, err, "build request")
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "POST %s body=%s", path, rec.Body.String())
	var resp cleanupResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode cleanup response")
	return resp
}

// flowGroupHasMember walks the v1 members surface — the same list
// `tclaude agent groups members` renders — and reports whether convID
// is still in it.
func flowGroupHasMember(f *testharness.Flow, group, convID string) bool {
	for _, m := range f.ListGroupMembers(group) {
		if m.ConvID == convID {
			return true
		}
	}
	return false
}

// Scenario: the per-group 🧹 cleanup button. The browser POSTs the
// human-edited member list — and a careless human could leave an
// online member ticked. The daemon's own tmux re-check, not the
// client's selection, is what protects the live agent: it stays in
// the group, the offline one is removed.
func TestCleanup_Group_RemovesOfflineKeepsOnline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const offlineConv = "offl-1111-2222-3333-4444"
	const onlineConv = "onln-1111-2222-3333-4444"
	f.HaveConvWithTitle(offlineConv, "stale-worker")
	f.HaveConvWithTitle(onlineConv, "live-worker")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveGroup("squad")
	f.HaveMember("squad", offlineConv)
	f.HaveMember("squad", onlineConv)
	f.MarkOffline("tmux-offl")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/group",
		`{"group":"squad","members":["`+offlineConv+`","`+onlineConv+`"]}`)

	assert.Equal(t, 1, resp.Removed, "exactly the offline member removed")
	assert.Equal(t, 1, resp.Skipped, "the online member skipped by the tmux re-check")
	f.AssertNotGroupMember("squad", offlineConv)
	assert.True(t, flowGroupHasMember(f, "squad", onlineConv),
		"online member must survive cleanup")
}

// Scenario: an offline group OWNER is excluded by default — a cleanup
// run without the opt-in leaves it untouched. Ticking "include
// offline owners" both removes the membership AND strips the owner
// row, and the response warns that the group is now ownerless.
func TestCleanup_Group_OwnerExcludedUnlessOptedIn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const ownerConv = "ownr-1111-2222-3333-4444"
	f.HaveConvWithTitle(ownerConv, "boss")
	f.HaveAliveSession(ownerConv, "spwn-ownr", "tmux-ownr", "/tmp/ownr")
	g := f.HaveGroup("squad")
	f.HaveMember("squad", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"), "seed owner")
	f.MarkOffline("tmux-ownr")

	mux := agentd.BuildDashboardHandlerForTest()

	// Default pass: owner stays put.
	def := postCleanup(t, mux, "/api/cleanup/group",
		`{"group":"squad","members":["`+ownerConv+`"]}`)
	assert.Equal(t, 0, def.Removed, "owner not removed by default")
	assert.Equal(t, 1, def.Skipped, "owner reported skipped")
	assert.True(t, flowGroupHasMember(f, "squad", ownerConv), "owner still a member")

	// Opt-in pass: owner removed, owner row gone, ownerless warning.
	inc := postCleanup(t, mux, "/api/cleanup/group",
		`{"group":"squad","members":["`+ownerConv+`"],"include_owners":true}`)
	assert.Equal(t, 1, inc.Removed, "owner removed with include_owners")
	f.AssertNotGroupMember("squad", ownerConv)
	isOwner, err := db.IsAgentGroupOwner(g.ID, ownerConv)
	require.NoError(t, err)
	assert.False(t, isOwner, "owner row must be stripped too")
	assert.NotEmpty(t, inc.Warnings, "expected an ownerless-group warning")
}

// Scenario: the Agents-tab 🧹 cleanup button — delete=true. Offline
// agents are purged (conv + every group/owner/perm row); an online
// agent in the same request is skipped by the tmux re-check.
func TestCleanup_Agents_DeleteOfflineSkipsOnline(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const offlineConv = "offl-1111-2222-3333-4444"
	const onlineConv = "onln-1111-2222-3333-4444"
	f.HaveConvWithTitle(offlineConv, "stale-worker")
	f.HaveConvWithTitle(onlineConv, "live-worker")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveGroup("squad")
	f.HaveMember("squad", offlineConv)
	f.HaveMember("squad", onlineConv)
	f.MarkOffline("tmux-offl")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+offlineConv+`","`+onlineConv+`"],"delete":true}`)

	assert.Equal(t, 1, resp.Deleted, "offline agent purged")
	assert.Equal(t, 1, resp.Skipped, "online agent skipped")
	f.AssertDeleted(offlineConv)
	f.AssertNotGroupMember("squad", offlineConv)
	assert.True(t, flowGroupHasMember(f, "squad", onlineConv),
		"online agent untouched")
}

// Scenario: the Groups-tab "clean up all groups" button with delete
// left OFF — an offline agent is unjoined from every group it
// belongs to but its conversation history is left intact on disk.
func TestCleanup_Agents_RemoveFromAllGroupsKeepsConv(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "many-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "rover")
	f.HaveAliveSession(conv, "spwn-many", "tmux-many", "/tmp/many")
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", conv)
	f.HaveMember("beta", conv)
	f.MarkOffline("tmux-many")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"delete":false}`)

	assert.Equal(t, 1, resp.Removed, "agent removed from its groups")
	assert.Equal(t, 0, resp.Deleted, "delete=false must not purge")
	f.AssertNotGroupMember("alpha", conv)
	f.AssertNotGroupMember("beta", conv)
	// The conv itself survives — only memberships were dropped.
	row, err := db.GetConvIndex(conv)
	require.NoError(t, err)
	assert.NotNil(t, row, "conv_index row must survive a remove-from-groups cleanup")
}

// Scenario: a cleanup pointed at a group that doesn't exist returns
// 404 — keeps the modal's error toast readable instead of silently
// reporting "0 removed".
func TestCleanup_Group_UnknownGroupReturns404(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	mux := agentd.BuildDashboardHandlerForTest()
	r, err := http.NewRequest(http.MethodPost, "/api/cleanup/group",
		strings.NewReader(`{"group":"no-such-group","members":["x"]}`))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// --- worktree cleanup ----------------------------------------------

// fakeWorktrees stands in for the git-worktree seam: inspect reports a
// canned WorktreeStatus per directory, remove records every removal so
// a test can assert which worktrees were (and were not) touched.
type fakeWorktrees struct {
	byDir map[string]worktree.WorktreeStatus
	// mu guards removed/branchRemoved: the deferred retire path records
	// removals from a background goroutine (scheduleRetireWorktreeCleanup)
	// while the test goroutine polls wasRemoved/branchesRemoved, so every
	// access to those slices must be synchronized.
	mu sync.Mutex
	// removed records every root passed to either remove seam (delete or
	// retire). branchRemoved is the branch arg the retire seam received,
	// in lockstep — empty entries for delete-path removals.
	removed       []string
	branchRemoved []string
	// removeErr, when set, makes the retire (branch-aware) seam report a
	// git failure — lets a flow test drive the failure-notice path.
	removeErr error
}

func (f *fakeWorktrees) inspect(dir string) worktree.WorktreeStatus {
	if st, ok := f.byDir[dir]; ok {
		return st
	}
	return worktree.WorktreeStatus{Kind: "none"}
}

func (f *fakeWorktrees) remove(root string, _ bool) (bool, error) {
	f.mu.Lock()
	f.removed = append(f.removed, root)
	f.branchRemoved = append(f.branchRemoved, "")
	f.mu.Unlock()
	return true, nil
}

// removeBranch is the retire-path (branch-aware) seam. It records the
// branch it was asked to delete and reports branchDeleted for any
// non-empty, non-protected branch — mirroring the real helper's
// main/master guard so a flow test can assert the trunk is never swept.
func (f *fakeWorktrees) removeBranch(root, branch string, _ bool) (bool, bool, error) {
	f.mu.Lock()
	f.removed = append(f.removed, root)
	f.branchRemoved = append(f.branchRemoved, branch)
	removeErr := f.removeErr
	f.mu.Unlock()
	if removeErr != nil {
		return false, false, removeErr
	}
	deleted := branch != "" && strings.ToLower(branch) != "main" && strings.ToLower(branch) != "master"
	return true, deleted, nil
}

func (f *fakeWorktrees) wasRemoved(root string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.removed {
		if r == root {
			return true
		}
	}
	return false
}

// branchesRemoved returns a snapshot of the branches passed to the
// removal seams. Callers must use this rather than reading the
// branchRemoved slice directly: a deferred retire cleanup may still be
// recording removals from a background goroutine.
func (f *fakeWorktrees) branchesRemoved() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.branchRemoved...)
}

// installFakeWorktrees swaps the agentd worktree seams for the test so
// worktree cleanup runs without real git repos. byDir maps an agent's
// cwd → the worktree status the fake reports for it. Both the delete
// (remove) and retire (removeBranch) seams are wired so either flow can
// use the same helper.
func installFakeWorktrees(t *testing.T, byDir map[string]worktree.WorktreeStatus) *fakeWorktrees {
	t.Helper()
	fw := &fakeWorktrees{byDir: byDir}
	t.Cleanup(agentd.SetWorktreeFnsForTest(fw.inspect, fw.remove))
	t.Cleanup(agentd.SetRetireWorktreeFnForTest(fw.removeBranch))
	return fw
}

// Scenario: deleting an offline agent with delete_worktrees set also
// removes the linked git worktree it was working in.
func TestCleanup_Agents_DeleteRemovesLinkedWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "wtre-1111-2222-3333-4444"
	const cwd = "/tmp/wt-linked"
	f.HaveConvWithTitle(conv, "worktree-worker")
	f.HaveAliveSession(conv, "spwn-wtre", "tmux-wtre", cwd)
	f.MarkOffline("tmux-wtre")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"delete":true,"delete_worktrees":true}`)

	assert.Equal(t, 1, resp.Deleted)
	assert.True(t, fw.wasRemoved(cwd), "linked worktree should be removed")
	require.Len(t, resp.Outcomes, 1)
	assert.Contains(t, resp.Outcomes[0].Detail, "worktree removed")
	f.AssertDeleted(conv)
}

// Scenario: a worktree a *surviving* agent still works in is never
// removed, even when one of its sharers is being deleted.
func TestCleanup_Agents_KeepsSharedWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const leaving = "wdel-1111-2222-3333-4444"
	const staying = "wsur-1111-2222-3333-4444"
	const shared = "/tmp/wt-shared"
	f.HaveConvWithTitle(leaving, "leaving")
	f.HaveConvWithTitle(staying, "staying")
	f.HaveAliveSession(leaving, "spwn-wdel", "tmux-wdel", shared)
	f.HaveAliveSession(staying, "spwn-wsur", "tmux-wsur", shared)
	f.MarkOffline("tmux-wdel")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		shared: {Root: shared, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+leaving+`"],"delete":true,"delete_worktrees":true}`)

	assert.Equal(t, 1, resp.Deleted)
	assert.False(t, fw.wasRemoved(shared),
		"a worktree another agent still works in must be kept")
	assert.Contains(t, resp.Outcomes[0].Detail, "shared")
}

// Scenario: the repo's main worktree is never removed by cleanup.
func TestCleanup_Agents_KeepsMainWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "wmai-1111-2222-3333-4444"
	const cwd = "/tmp/wt-main"
	f.HaveConvWithTitle(conv, "main-repo-worker")
	f.HaveAliveSession(conv, "spwn-wmai", "tmux-wmai", cwd)
	f.MarkOffline("tmux-wmai")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "main", Kind: "main"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"delete":true,"delete_worktrees":true}`)

	assert.Equal(t, 1, resp.Deleted)
	assert.False(t, fw.wasRemoved(cwd), "the main worktree must never be removed")
	assert.Contains(t, resp.Outcomes[0].Detail, "main repo")
}

// Scenario: delete_worktrees defaults off — a delete without it leaves
// the worktree alone.
func TestCleanup_Agents_DeleteWorktreesOptIn(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "wopt-1111-2222-3333-4444"
	const cwd = "/tmp/wt-opt"
	f.HaveConvWithTitle(conv, "opt-worker")
	f.HaveAliveSession(conv, "spwn-wopt", "tmux-wopt", cwd)
	f.MarkOffline("tmux-wopt")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"delete":true,"delete_worktrees":false}`)

	assert.Equal(t, 1, resp.Deleted)
	assert.False(t, fw.wasRemoved(cwd), "worktree untouched when delete_worktrees=false")
	assert.NotContains(t, resp.Outcomes[0].Detail, "worktree")
}

// Scenario: the retire tier honours delete_worktrees too (the command
// palette's "Retire ungrouped agents…" sweep ticks it on by default).
// An offline agent is demoted AND its linked worktree removed inline —
// reusing the single-agent retire machinery, not the delete path.
func TestCleanup_Agents_RetireRemovesLinkedWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwtr-1111-2222-3333-4444"
	const cwd = "/tmp/wt-retire"
	f.HaveConvWithTitle(conv, "loose-worker")
	f.HaveEnrolledAgent(conv) // retire acts only on an active agent
	f.HaveAliveSession(conv, "spwn-rwtr", "tmux-rwtr", cwd)
	f.MarkOffline("tmux-rwtr")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"retire","delete_worktrees":true}`)

	assert.Equal(t, 1, resp.Retired, "the offline agent is demoted")
	assert.Equal(t, 0, resp.Failed)
	require.Len(t, resp.Outcomes, 1)
	assert.Equal(t, "retired", resp.Outcomes[0].Result)
	assert.True(t, fw.wasRemoved(cwd), "the retired agent's linked worktree is removed")
	assert.Contains(t, resp.Outcomes[0].Detail, "worktree + branch feat removed",
		"the worktree plan note is folded into the outcome detail")
	// The demotion actually took: the conv is a retired conversation now.
	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state)
}

// Scenario: two offline agents share a worktree and are retired together.
// Their safety views must come from one pre-retire snapshot; otherwise the
// first demotion disappears from the active roster before the second is
// processed, and the later target can incorrectly remove their shared tree.
func TestCleanup_Agents_RetireBatchKeepsCoSharedWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const convA = "rwba-1111-2222-3333-4444"
	const convB = "rwbb-1111-2222-3333-4444"
	const shared = "/tmp/wt-retire-batch-shared"
	for _, c := range []struct {
		conv, label, tmux string
	}{
		{convA, "spwn-rwba", "tmux-rwba"},
		{convB, "spwn-rwbb", "tmux-rwbb"},
	} {
		f.HaveConvWithTitle(c.conv, "batch-worker")
		f.HaveAliveSession(c.conv, c.label, c.tmux, shared)
		f.HaveEnrolledAgent(c.conv)
		f.MarkOffline(c.tmux)
	}
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		shared: {Root: shared, Branch: "shared", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+convA+`","`+convB+`"],"mode":"retire","delete_worktrees":true}`)

	assert.Equal(t, 2, resp.Retired)
	assert.False(t, fw.wasRemoved(shared), "a co-shared worktree must be kept for the whole retire cohort")
	require.Len(t, resp.Outcomes, 2)
	for _, out := range resp.Outcomes {
		assert.Contains(t, out.Detail, "shared", "each cohort member must receive the stable shared decision")
	}
}

// Scenario: retire without delete_worktrees leaves the worktree alone —
// the box is coupled to shutdown and defaults on, but an unticked box
// (or an older client) must never nuke a worktree by accident.
func TestCleanup_Agents_RetireKeepsWorktreeWithoutFlag(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rwtk-1111-2222-3333-4444"
	const cwd = "/tmp/wt-retire-keep"
	f.HaveConvWithTitle(conv, "loose-worker")
	f.HaveEnrolledAgent(conv)
	f.HaveAliveSession(conv, "spwn-rwtk", "tmux-rwtk", cwd)
	f.MarkOffline("tmux-rwtk")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"retire"}`)

	assert.Equal(t, 1, resp.Retired)
	assert.False(t, fw.wasRemoved(cwd), "worktree untouched when delete_worktrees is absent")
	assert.NotContains(t, resp.Outcomes[0].Detail, "worktree")
}

// Scenario: the per-row delete button's path — GET .../worktree
// classifies the agent's worktree (what the modal checkbox reads),
// and DELETE ?delete_worktree=1 removes it alongside the agent.
func TestDeleteAgent_WithWorktree(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "wsng-1111-2222-3333-4444"
	const cwd = "/tmp/wt-single"
	f.HaveConvWithTitle(conv, "solo")
	f.HaveAliveSession(conv, "spwn-wsng", "tmux-wsng", cwd)
	f.MarkOffline("tmux-wsng")
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})
	mux := agentd.BuildDashboardHandlerForTest()

	// GET worktree info — what the delete-agent modal reads to decide
	// whether to show (and enable) its checkbox.
	grec := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/worktree", nil))
	require.Equal(t, http.StatusOK, grec.Code, "body=%s", grec.Body.String())
	var wtInfo struct {
		Kind      string `json:"kind"`
		Path      string `json:"path"`
		Branch    string `json:"branch"`
		Shared    bool   `json:"shared"`
		Removable bool   `json:"removable"`
	}
	require.NoError(t, json.Unmarshal(grec.Body.Bytes(), &wtInfo))
	assert.Equal(t, "linked", wtInfo.Kind)
	assert.True(t, wtInfo.Removable, "a lone linked worktree is removable")
	assert.False(t, wtInfo.Shared)

	// DELETE with ?delete_worktree=1 — the modal's confirm path.
	dr, err := http.NewRequest(http.MethodDelete,
		"/api/agents/"+conv+"?delete_worktree=1", nil)
	require.NoError(t, err)
	drec := testharness.Serve(mux, dr)
	require.Equal(t, http.StatusOK, drec.Code, "body=%s", drec.Body.String())
	assert.True(t, fw.wasRemoved(cwd), "worktree removed on ?delete_worktree=1")
	f.AssertDeleted(conv)
}

// The delete dialog probes a removable worktree before the human confirms.
// The DELETE must fail closed if the agent moves to another worktree in that
// gap: neither the agent nor either worktree may be removed from stale UI
// state. A matching, URL-encoded path remains the successful opt-in contract.
func TestDeleteAgent_WorktreePrecondition(t *testing.T) {
	t.Run("moved after probe conflicts before deletion", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "wtpc-1111-2222-3333-4444"
		const pathA = "/tmp/wt-precondition-a"
		const pathB = "/tmp/wt-precondition-b"
		f.HaveConvWithTitle(conv, "moving-worker")
		f.HaveAliveSession(conv, "spwn-wtpc", "tmux-wtpc", pathA)
		f.MarkOffline("tmux-wtpc")
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			pathA: {Root: pathA, Branch: "feature-a", Kind: "linked"},
			pathB: {Root: pathB, Branch: "feature-b", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		probe := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/worktree", nil))
		require.Equal(t, http.StatusOK, probe.Code, "body=%s", probe.Body.String())
		assert.Contains(t, probe.Body.String(), pathA)

		require.NoError(t, db.UpsertAgentWorkdir(conv, pathB, pathB, "feature-b"))
		query := url.Values{
			"delete_worktree":   {"1"},
			"expected_worktree": {pathA},
		}.Encode()
		req, err := http.NewRequest(http.MethodDelete, "/api/agents/"+conv+"?"+query, nil)
		require.NoError(t, err)
		resp := testharness.Serve(mux, req)
		require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "worktree changed")
		assert.Contains(t, resp.Body.String(), "retry")

		sessionRow, err := db.FindSessionByConvID(conv)
		require.NoError(t, err)
		require.NotNil(t, sessionRow, "agent session must survive the conflict")
		convRow, err := db.GetConvIndex(conv)
		require.NoError(t, err)
		require.NotNil(t, convRow, "conversation must survive the conflict")
		assert.False(t, fw.wasRemoved(pathA), "stale probed worktree must survive")
		assert.False(t, fw.wasRemoved(pathB), "current worktree must survive")
	})

	t.Run("matching encoded path deletes", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "wtpd-1111-2222-3333-4444"
		const cwd = "/tmp/wt precondition & exact?#"
		f.HaveConvWithTitle(conv, "steady-worker")
		f.HaveAliveSession(conv, "spwn-wtpd", "tmux-wtpd", cwd)
		f.MarkOffline("tmux-wtpd")
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "feature-safe", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"delete_worktree":   {"1"},
			"expected_worktree": {cwd},
		}.Encode()
		req, err := http.NewRequest(http.MethodDelete, "/api/agents/"+conv+"?"+query, nil)
		require.NoError(t, err)
		resp := testharness.Serve(mux, req)
		require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())
		assert.True(t, fw.wasRemoved(cwd), "decoded matching worktree must be removed")
		f.AssertDeleted(conv)
	})

	t.Run("precondition without opt-in is rejected", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "wtpu-1111-2222-3333-4444"
		const cwd = "/tmp/wt-unexpected-precondition"
		f.HaveConvWithTitle(conv, "guarded-worker")
		f.HaveAliveSession(conv, "spwn-wtpu", "tmux-wtpu", cwd)
		f.MarkOffline("tmux-wtpu")
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "feature-guarded", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{"expected_worktree": {cwd}}.Encode()
		req, err := http.NewRequest(http.MethodDelete, "/api/agents/"+conv+"?"+query, nil)
		require.NoError(t, err)
		resp := testharness.Serve(mux, req)
		require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "requires delete_worktree=1")

		sessionRow, err := db.FindSessionByConvID(conv)
		require.NoError(t, err)
		require.NotNil(t, sessionRow, "agent must survive an invalid precondition")
		assert.False(t, fw.wasRemoved(cwd))
	})
}

// --- category coverage: retired agents & plain conversations -------
//
// The Agents-tab cleanup modal used to build its candidate list from
// active agents alone. These scenarios pin the daemon side of the
// widened tool: the delete tier reaches every category, the new
// reinstate tier is the inverse of retire, and a tier that doesn't
// apply to a target skips it gracefully instead of failing.

// Scenario (JOH-31): the dashboard's "Delete retired agents…" preview
// modal (openDeleteRetiredPreview) is built on the load-bearing invariant
// that ONLY the rows that are both ticked AND visible under the current
// filters are POSTed — verbatim, never re-resolved server-side. This pins
// the daemon half of that contract: when the browser submits an explicit
// subset of the retired population, /api/cleanup/agents {mode:"delete"}
// deletes EXACTLY that subset and leaves the rest of the retired roster
// untouched. A retired agent that the human had filtered out (or unticked)
// is simply absent from the `agents` list, so it must survive — verified
// at the endpoint/DB surface, not by poking the .jsonl.
func TestCleanup_Agents_DeleteRetired_ExplicitSubsetOnly(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	// Three retired agents in the population. The "ticked & visible" set is
	// {keep-typo-a, keep-typo-b}; `filtered` stands for a row the human had
	// ticked but then hid behind a name/age filter, so it is NOT in the POST.
	const deleteA = "dela-1111-2222-3333-4444"
	const deleteB = "delb-1111-2222-3333-4444"
	const filtered = "keep-1111-2222-3333-4444"
	for conv, title := range map[string]string{
		deleteA:  "stale-alpha",
		deleteB:  "stale-bravo",
		filtered: "still-wanted",
	} {
		f.HaveConvWithTitle(conv, title)
		f.HaveRetiredAgent(conv)
	}

	mux := agentd.BuildDashboardHandlerForTest()
	// The browser POSTs exactly the snapshotted ticked-and-visible list —
	// deleteA + deleteB only; `filtered` is deliberately omitted.
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+deleteA+`","`+deleteB+`"],"mode":"delete"}`)

	assert.Equal(t, 2, resp.Deleted, "exactly the two POSTed retired agents purged")
	assert.Equal(t, 0, resp.Failed)
	f.AssertDeleted(deleteA)
	f.AssertDeleted(deleteB)
	for _, conv := range []string{deleteA, deleteB} {
		state, err := db.AgentState(conv)
		require.NoError(t, err)
		assert.Equalf(t, db.AgentStateNone, state, "enrollment row gone for %s", conv)
	}

	// The filtered-out retired agent was never in the POST, so the endpoint
	// never touched it — it is still a retired conversation on disk.
	state, err := db.AgentState(filtered)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state,
		"a retired agent absent from the POST list must survive — the BE acts on the explicit list, never the whole population")
	row, err := db.GetConvIndex(filtered)
	require.NoError(t, err)
	require.NotNil(t, row, "filtered-out conv_index row kept")
	for _, o := range resp.Outcomes {
		assert.NotEqualf(t, filtered, o.ConvID, "filtered-out agent must not appear in the outcome log")
	}
}

// Scenario: the delete tier reaches a retired agent — a category the
// modal previously couldn't see. The retired enrollment row is purged
// alongside the conversation.
func TestCleanup_Agents_DeleteRetiredAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "retd-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "demoted-worker")
	f.HaveRetiredAgent(conv)
	f.HaveAliveSession(conv, "spwn-retd", "tmux-retd", "/tmp/retd")
	f.MarkOffline("tmux-retd")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"delete"}`)

	assert.Equal(t, 1, resp.Deleted, "retired agent purged")
	f.AssertDeleted(conv)
	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateNone, state, "enrollment row gone after delete")
}

// Scenario: the delete tier also reaches a plain (never-enrolled)
// conversation — the third category the old modal ignored.
func TestCleanup_Agents_DeletePlainConversation(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "plan-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "just-a-chat")
	f.HaveAliveSession(conv, "spwn-plan", "tmux-plan", "/tmp/plan")
	f.MarkOffline("tmux-plan")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"delete"}`)

	assert.Equal(t, 1, resp.Deleted, "plain conversation purged")
	f.AssertDeleted(conv)
	row, err := db.GetConvIndex(conv)
	require.NoError(t, err)
	assert.Nil(t, row, "conv_index row gone after delete")
}

// Scenario: the reinstate tier — the inverse of retire — returns a
// retired agent to the active roster in bulk.
func TestCleanup_Agents_ReinstateRetiredAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "rein-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "comeback")
	f.HaveRetiredAgent(conv)

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"reinstate"}`)

	assert.Equal(t, 1, resp.Reinstated, "retired agent reinstated")
	require.Len(t, resp.Outcomes, 1)
	assert.Equal(t, "reinstated", resp.Outcomes[0].Result)
	state, err := db.AgentState(conv)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "back on the active roster")
}

// Scenario: reinstate is a graceful no-op skip for a target that isn't
// retired — an active agent caught in a mixed selection.
func TestCleanup_Agents_ReinstateSkipsActiveAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "actv-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "still-working")
	f.HaveEnrolledAgent(conv)

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"reinstate"}`)

	assert.Equal(t, 0, resp.Reinstated)
	assert.Equal(t, 1, resp.Skipped, "active agent skipped — nothing to reinstate")
	require.Len(t, resp.Outcomes, 1)
	assert.Contains(t, resp.Outcomes[0].Detail, "not a retired agent")
}

// Scenario: the retire tier gracefully skips a target that is already
// retired — so a mixed-category selection never errors.
func TestCleanup_Agents_RetireSkipsRetiredAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "altd-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "already-demoted")
	f.HaveRetiredAgent(conv)

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"retire"}`)

	assert.Equal(t, 0, resp.Retired)
	assert.Equal(t, 1, resp.Skipped, "already retired — nothing to retire")
}

// Scenario: include_online lifts the skip-online guard. Without it a
// running conversation is skipped; with it the session is force-stopped
// and the conversation deleted.
func TestCleanup_Agents_IncludeOnlineDeletesRunning(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const conv = "live-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "running-chat")
	f.HaveAliveSession(conv, "spwn-live", "tmux-live", "/tmp/live")

	mux := agentd.BuildDashboardHandlerForTest()

	// Default: online → skipped, untouched.
	skip := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"delete"}`)
	assert.Equal(t, 0, skip.Deleted, "online conv not deleted without opt-in")
	assert.Equal(t, 1, skip.Skipped, "online conv skipped")

	// Opt-in: include_online → force-stopped and deleted.
	del := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+conv+`"],"mode":"delete","include_online":true}`)
	assert.Equal(t, 1, del.Deleted, "online conv deleted with include_online")
	f.AssertDeleted(conv)
}

// Scenario: one delete pass spanning all three categories — an active
// agent, a retired agent and a plain conversation — purges every one.
func TestCleanup_Agents_MixedCategoriesDelete(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const active = "amix-1111-2222-3333-4444"
	const retired = "rmix-1111-2222-3333-4444"
	const plain = "pmix-1111-2222-3333-4444"
	f.HaveConvWithTitle(active, "active-one")
	f.HaveConvWithTitle(retired, "retired-one")
	f.HaveConvWithTitle(plain, "plain-one")
	f.HaveEnrolledAgent(active)
	f.HaveRetiredAgent(retired)
	f.HaveAliveSession(active, "spwn-amix", "tmux-amix", "/tmp/amix")
	f.HaveAliveSession(retired, "spwn-rmix", "tmux-rmix", "/tmp/rmix")
	f.HaveAliveSession(plain, "spwn-pmix", "tmux-pmix", "/tmp/pmix")
	f.MarkOffline("tmux-amix")
	f.MarkOffline("tmux-rmix")
	f.MarkOffline("tmux-pmix")

	mux := agentd.BuildDashboardHandlerForTest()
	resp := postCleanup(t, mux, "/api/cleanup/agents",
		`{"agents":["`+active+`","`+retired+`","`+plain+`"],"mode":"delete"}`)

	assert.Equal(t, 3, resp.Deleted, "all three categories purged")
	f.AssertDeleted(active)
	f.AssertDeleted(retired)
	f.AssertDeleted(plain)
}
