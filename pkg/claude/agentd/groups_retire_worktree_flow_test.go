package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the BATCH parallel of the single-agent retire
// worktree option: a bulk groups.retire can also remove each retired
// member's git worktree AND delete its branch when delete_worktree is
// set. It reuses the single-agent machinery per member
// (resolveRetireWorktree before the shutdown, scheduleRetireWorktreeCleanup
// after), so the same safety rules hold — the main repo and worktrees a
// surviving agent still works in are kept, and the removal runs only after
// each pane exits. These scenarios drive the cookie-authed dashboard route
// (the preview modal's path) and the SO_PEERCRED /v1 route, asserting the
// worktree seam was (or wasn't) hit per member.

// wtGroupRetireResp decodes the bulk-retire response WITH the per-member
// worktree sub-object the delete_worktree path attaches (the shared
// groupRetireResp test type omits it).
type wtGroupRetireResp struct {
	Members []struct {
		ConvID   string `json:"conv_id"`
		Action   string `json:"action"`
		Detail   string `json:"detail"`
		Worktree *struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"worktree"`
	} `json:"members"`
}

func (r wtGroupRetireResp) member(conv string) (action, detail string, wtAction string, hasWt bool) {
	for _, m := range r.Members {
		if m.ConvID == conv {
			if m.Worktree != nil {
				return m.Action, m.Detail, m.Worktree.Action, true
			}
			return m.Action, m.Detail, "", false
		}
	}
	return "", "", "", false
}

// postDashGroupRetireWt POSTs a JSON body to the cookie-authed dashboard
// retire route and decodes the worktree-aware response.
func postDashGroupRetireWt(t *testing.T, mux http.Handler, group string, body map[string]any) (int, wtGroupRetireResp) {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/retire", body)
	rec := testharness.Serve(mux, r)
	var resp wtGroupRetireResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode batch retire response: %s", rec.Body.String())
	}
	return rec.Code, resp
}

// Scenario: a batch retire with delete_worktree removes EACH selected
// member's linked worktree and deletes its branch. Both agents are alive
// and shutdown defaults on; the sim's /exit is synchronous so each is
// offline by the time cleanup runs — the removal happens inline and the
// per-member response reports it.
func TestDashboardGroupRetire_DeleteWorktreeRemovesEach(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "wt-batch"
	const convA = "bwaa-1111-2222-3333-4444"
	const convB = "bwbb-1111-2222-3333-4444"
	const cwdA = "/tmp/bw-a"
	const cwdB = "/tmp/bw-b"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "wt-worker-a")
	f.HaveConvWithTitle(convB, "wt-worker-b")
	f.HaveAliveSession(convA, "spwn-bwaa", "tmux-bwaa", cwdA)
	f.HaveAliveSession(convB, "spwn-bwbb", "tmux-bwbb", cwdB)
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwdA: {Root: cwdA, Branch: "feat-a", Kind: "linked"},
		cwdB: {Root: cwdB, Branch: "feat-b", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postDashGroupRetireWt(t, mux, group, map[string]any{
		"convs": []string{convA, convB}, "shutdown": true, "delete_worktree": true,
	})
	require.Equal(t, http.StatusOK, code)

	for _, c := range []struct{ conv, cwd, branch string }{
		{convA, cwdA, "feat-a"}, {convB, cwdB, "feat-b"},
	} {
		action, _, wtAction, hasWt := resp.member(c.conv)
		assert.Equal(t, "retired", action, "%s must be retired; members=%+v", c.conv, resp.Members)
		require.True(t, hasWt, "%s must carry a worktree outcome when delete_worktree is set", c.conv)
		assert.Equal(t, "removed", wtAction, "%s's already-offline worktree is removed inline", c.conv)
		assert.True(t, fw.wasRemoved(c.cwd), "%s's linked worktree must be removed", c.conv)
		assert.Contains(t, fw.branchesRemoved(), c.branch, "%s's branch must be deleted", c.conv)
		state, err := db.EnrollmentState(c.conv)
		require.NoError(t, err)
		assert.Equal(t, db.EnrollmentRetired, state, "%s must be retired", c.conv)
	}
}

// Scenario: a batch retire WITHOUT delete_worktree leaves every worktree
// untouched and attaches no worktree outcome — the failsafe default.
func TestDashboardGroupRetire_NoDeleteWorktreeLeavesUntouched(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "wt-none"
	const conv = "bwnn-1111-2222-3333-4444"
	const cwd = "/tmp/bw-none"
	f.HaveGroup(group)
	f.HaveConvWithTitle(conv, "kept-wt")
	f.HaveAliveSession(conv, "spwn-bwnn", "tmux-bwnn", cwd)
	f.HaveMember(group, conv)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	// No delete_worktree key → the default OFF failsafe.
	code, resp := postDashGroupRetireWt(t, mux, group, map[string]any{
		"convs": []string{conv}, "shutdown": true,
	})
	require.Equal(t, http.StatusOK, code)

	action, _, _, hasWt := resp.member(conv)
	assert.Equal(t, "retired", action)
	assert.False(t, hasWt, "no delete_worktree → no per-member worktree outcome")
	assert.False(t, fw.wasRemoved(cwd), "the worktree must be untouched without delete_worktree")
}

// Scenario: in a batch with delete_worktree, the three safety branches all
// fire — a member on the MAIN repo keeps it, a member SHARING a worktree
// with a surviving (unselected) agent keeps it, and a member with its own
// LINKED worktree has it removed. Proves the per-member safety rules of the
// single-agent retire carry into the batch.
func TestDashboardGroupRetire_DeleteWorktreeKeepsMainAndShared(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "wt-safety"
	const linked = "bwsl-1111-2222-3333-4444"  // own linked worktree → removed
	const onMain = "bwsm-1111-2222-3333-4444"  // cwd is the main repo → kept
	const sharer = "bwsh-1111-2222-3333-4444"  // shares with a survivor → kept
	const survive = "bwsv-1111-2222-3333-4444" // NOT selected; keeps sharer's worktree alive
	const cwdLinked = "/tmp/bw-linked"
	const cwdMain = "/tmp/bw-main"
	const cwdShared = "/tmp/bw-shared"
	f.HaveGroup(group)
	f.HaveConvWithTitle(linked, "linked-worker")
	f.HaveConvWithTitle(onMain, "main-worker")
	f.HaveConvWithTitle(sharer, "sharer-worker")
	f.HaveConvWithTitle(survive, "surviving-worker")
	f.HaveAliveSession(linked, "spwn-bwsl", "tmux-bwsl", cwdLinked)
	f.HaveAliveSession(onMain, "spwn-bwsm", "tmux-bwsm", cwdMain)
	f.HaveAliveSession(sharer, "spwn-bwsh", "tmux-bwsh", cwdShared)
	f.HaveAliveSession(survive, "spwn-bwsv", "tmux-bwsv", cwdShared) // same dir as sharer
	f.HaveMember(group, linked)
	f.HaveMember(group, onMain)
	f.HaveMember(group, sharer)
	f.HaveMember(group, survive)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwdLinked: {Root: cwdLinked, Branch: "feat", Kind: "linked"},
		cwdMain:   {Root: cwdMain, Branch: "main", Kind: "main"},
		cwdShared: {Root: cwdShared, Branch: "shared", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	// Select the three workers; leave `survive` out so it keeps cwdShared live.
	code, resp := postDashGroupRetireWt(t, mux, group, map[string]any{
		"convs": []string{linked, onMain, sharer}, "shutdown": true, "delete_worktree": true,
	})
	require.Equal(t, http.StatusOK, code)

	_, _, linkedWt, _ := resp.member(linked)
	assert.Equal(t, "removed", linkedWt, "the own-linked worktree is removed; members=%+v", resp.Members)
	assert.True(t, fw.wasRemoved(cwdLinked), "the linked worktree must be removed")

	_, mainDetail, mainWt, _ := resp.member(onMain)
	assert.Equal(t, "kept", mainWt, "the main repo is never removed; members=%+v", resp.Members)
	assert.Contains(t, mainDetail, "main repo")
	assert.False(t, fw.wasRemoved(cwdMain), "the main repo must never be removed")

	_, sharedDetail, sharedWt, _ := resp.member(sharer)
	assert.Equal(t, "kept", sharedWt, "a worktree a survivor still uses is kept; members=%+v", resp.Members)
	assert.Contains(t, sharedDetail, "shared")
	assert.False(t, fw.wasRemoved(cwdShared), "the shared worktree must be kept while a survivor uses it")

	// The unselected survivor is fully intact.
	survState, err := db.EnrollmentState(survive)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, survState, "the unselected survivor stays active")
	assert.True(t, f.World.Tmux.IsAlive("tmux-bwsv"), "the survivor's pane must not be touched")
}

// Scenario: TWO members that BOTH share one worktree and are BOTH selected
// in the same batch retire — the worktree is conservatively KEPT for both.
// Each still sees the other's session row for that root (rows outlive a
// soft-exit), so the shared check marks it shared either way. This is the
// safe failure mode the batch doc promises: never remove a worktree out
// from under a co-retired sibling whose pane is still draining. (If a
// future change made soft-exit delete the session row, this test would
// fail loudly — the regression guard for that latent race.)
func TestDashboardGroupRetire_DeleteWorktreeBothShareKept(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "wt-coshare"
	const convA = "bwca-1111-2222-3333-4444"
	const convB = "bwcb-1111-2222-3333-4444"
	const shared = "/tmp/bw-coshare"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "co-a")
	f.HaveConvWithTitle(convB, "co-b")
	f.HaveAliveSession(convA, "spwn-bwca", "tmux-bwca", shared)
	f.HaveAliveSession(convB, "spwn-bwcb", "tmux-bwcb", shared) // same dir
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		shared: {Root: shared, Branch: "shared", Kind: "linked"},
	})

	mux := agentd.BuildDashboardHandlerForTest()
	code, resp := postDashGroupRetireWt(t, mux, group, map[string]any{
		"convs": []string{convA, convB}, "shutdown": true, "delete_worktree": true,
	})
	require.Equal(t, http.StatusOK, code)

	for _, c := range []string{convA, convB} {
		action, detail, wtAction, hasWt := resp.member(c)
		assert.Equal(t, "retired", action, "%s must be retired; members=%+v", c, resp.Members)
		require.True(t, hasWt, "%s must carry a worktree outcome", c)
		assert.Equal(t, "kept", wtAction,
			"a worktree two co-retired members share is conservatively kept; %s detail=%s", c, detail)
		assert.Contains(t, detail, "shared")
	}
	assert.False(t, fw.wasRemoved(shared),
		"the co-shared worktree must never be removed while a sibling pane may still be draining")
}

// Scenario: the SO_PEERCRED /v1 route honours ?delete_worktree=1 — the
// coordinator path. A slug-holding agent bulk-retires a worker and its
// worktree+branch are swept, proving the option is wired on both retire
// surfaces, not just the dashboard.
func TestGroupRetire_V1DeleteWorktreeQuery(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const group = "wt-v1"
	const caller = "bwvc-1111-2222-3333-4444"
	const worker = "bwvw-1111-2222-3333-4444"
	const cwd = "/tmp/bw-v1"
	f.HaveGroup(group)
	f.HaveConvWithTitle(caller, "coordinator")
	f.HaveConvWithTitle(worker, "v1-worker")
	f.HaveAliveSession(caller, "spwn-bwvc", "tmux-bwvc", "/tmp/bwvc")
	f.HaveAliveSession(worker, "spwn-bwvw", "tmux-bwvw", cwd)
	f.HaveMember(group, caller)
	f.HaveMember(group, worker)
	require.NoError(t, db.GrantAgentPermission(caller, "groups.retire", "human"))
	fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
		cwd: {Root: cwd, Branch: "feat", Kind: "linked"},
	})

	wrap := func(r *http.Request) *http.Request { return agentd.AsAgentPeer(r, caller) }
	path := "/v1/groups/" + group + "/retire?delete_worktree=1"
	rec := testharness.Serve(f.Mux, wrap(testharness.JSONRequest(t, http.MethodPost, path, nil)))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp wtGroupRetireResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode: %s", rec.Body.String())

	action, _, wtAction, hasWt := resp.member(worker)
	assert.Equal(t, "retired", action, "members=%+v", resp.Members)
	require.True(t, hasWt, "?delete_worktree=1 must attach a worktree outcome")
	assert.Equal(t, "removed", wtAction, "the worker's worktree is removed inline")
	assert.True(t, fw.wasRemoved(cwd), "the worker's worktree must be removed")
	assert.Contains(t, fw.branchesRemoved(), "feat", "the branch must be deleted")

	// The caller never retires (or sweeps) itself.
	_, _, _, callerHasWt := resp.member(caller)
	assert.False(t, callerHasWt, "the caller is skipped:self — no worktree action")
	assert.False(t, fw.wasRemoved("/tmp/bwvc"), "the caller's own dir must never be swept")
}
