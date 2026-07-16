package agentd_test

import (
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The retire dialog probes a removable worktree before the human confirms, and
// a failed request leaves that frozen dialog retryable. Retire must fail closed
// if the agent claims a different worktree in that gap: the operator reviewed
// path A, so a Retry can never demote the agent or sweep B and its branch off
// stale UI state. Mirrors the permanent-delete precondition contract.
func TestRetireAgent_WorktreePrecondition(t *testing.T) {
	t.Run("moved after probe conflicts before any mutation", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpc-1111-2222-3333-4444"
		const pathA = "/tmp/retire-precondition-a"
		const pathB = "/tmp/retire-precondition-b"
		f.HaveConvWithTitle(conv, "moving-worker")
		f.HaveAliveSession(conv, "spwn-rwpc", "tmux-rwpc", pathA)
		f.MarkOffline("tmux-rwpc")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			pathA: {Root: pathA, Branch: "feature-a", Kind: "linked"},
			pathB: {Root: pathB, Branch: "feature-b", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		probe := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/worktree", nil))
		require.Equal(t, http.StatusOK, probe.Code, "body=%s", probe.Body.String())
		assert.Contains(t, probe.Body.String(), pathA)

		// The first request fails in transport; the agent then moves to B while
		// the frozen dialog waits. Retry still names the reviewed worktree A.
		require.NoError(t, db.UpsertAgentWorkdir(conv, pathB, pathB, "feature-b"))
		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {pathA},
			"expected_branch":   {"feature-a"},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "changed since confirmation")
		assert.Contains(t, resp.Body.String(), "re-probe")

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.True(t, live, "a rejected precondition must never demote the agent")
		assert.False(t, fw.wasRemoved(pathA), "stale probed worktree must survive")
		assert.False(t, fw.wasRemoved(pathB), "the agent's new worktree must survive")
		assert.Empty(t, fw.branchesRemoved(), "no branch may be deleted behind a conflict")
	})

	t.Run("matching encoded path retires and sweeps the confirmed worktree", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpm-1111-2222-3333-4444"
		const cwd = "/tmp/retire precondition & exact?#"
		f.HaveConvWithTitle(conv, "steady-worker")
		f.HaveAliveSession(conv, "spwn-rwpm", "tmux-rwpm", cwd)
		f.MarkOffline("tmux-rwpm")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "feature-safe", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {cwd},
			"expected_branch":   {"feature-safe"},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.False(t, live, "a satisfied precondition retires normally")
		assert.True(t, fw.wasRemoved(cwd), "decoded matching worktree must be removed")
		assert.Contains(t, fw.branchesRemoved(), "feature-safe")
	})

	// The destructive half nobody sees coming: the agent never leaves the
	// confirmed path, it just switches branch in place. Retire force-deletes
	// the branch, and the confirmation row named feature-a, so feature-b and
	// any uncommitted work on it must survive a Retry.
	t.Run("same-path branch switch conflicts before any mutation", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpb-1111-2222-3333-4444"
		const cwd = "/tmp/retire-branch-switch"
		f.HaveConvWithTitle(conv, "branch-switching-worker")
		f.HaveAliveSession(conv, "spwn-rwpb", "tmux-rwpb", cwd)
		f.MarkOffline("tmux-rwpb")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "feature-a", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		probe := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/worktree", nil))
		require.Equal(t, http.StatusOK, probe.Code, "body=%s", probe.Body.String())
		assert.Contains(t, probe.Body.String(), "feature-a")

		// git switch feature-b — same worktree, same removability, new branch.
		fw.setDir(cwd, worktree.WorktreeStatus{
			Root: cwd, Branch: "feature-b", Kind: "linked",
		})
		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {cwd},
			"expected_branch":   {"feature-a"},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "changed since confirmation")

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.True(t, live, "an unreviewed branch must never cost the agent its retirement")
		assert.False(t, fw.wasRemoved(cwd), "the worktree must survive an unconfirmed branch")
		assert.NotContains(t, fw.branchesRemoved(), "feature-b",
			"a branch the operator never confirmed must never be force-deleted")
		assert.Empty(t, fw.branchesRemoved(), "no branch may be deleted behind a conflict")
	})

	// A detached HEAD freezes an empty branch. That is a bound value, not an
	// absent precondition, so it must satisfy the guard rather than trip it.
	t.Run("detached HEAD binds an explicitly empty branch", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpd-1111-2222-3333-4444"
		const cwd = "/tmp/retire-detached-head"
		f.HaveConvWithTitle(conv, "detached-worker")
		f.HaveAliveSession(conv, "spwn-rwpd", "tmux-rwpd", cwd)
		f.MarkOffline("tmux-rwpd")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {cwd},
			"expected_branch":   {""},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.False(t, live, "an empty branch is a satisfied precondition, not a missing one")
		assert.True(t, fw.wasRemoved(cwd), "a detached-HEAD worktree still deletes")
	})

	// A detached HEAD that gains a branch is still a change the operator never
	// reviewed — the empty precondition must not degrade into "any branch".
	t.Run("branch appearing under a detached-HEAD confirmation conflicts", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpg-1111-2222-3333-4444"
		const cwd = "/tmp/retire-detached-gained"
		f.HaveConvWithTitle(conv, "detached-gained-worker")
		f.HaveAliveSession(conv, "spwn-rwpg", "tmux-rwpg", cwd)
		f.MarkOffline("tmux-rwpg")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "rescued-work", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {cwd},
			"expected_branch":   {""},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Body.String())

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.True(t, live)
		assert.Empty(t, fw.branchesRemoved(), "a branch created after confirmation must survive")
	})

	// Half a precondition is not a precondition: the pair travels together or
	// not at all, so a caller can never bind the path while leaving the branch
	// free to be resolved fresh.
	t.Run("half a precondition pair is rejected", func(t *testing.T) {
		for _, row := range []struct {
			name  string
			query url.Values
		}{
			{"path without branch", url.Values{
				"shutdown": {"1"}, "delete_worktree": {"1"},
				"expected_worktree": {"/tmp/retire-half-pair"},
			}},
			{"branch without path", url.Values{
				"shutdown": {"1"}, "delete_worktree": {"1"},
				"expected_branch": {"feature-half"},
			}},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
				f := newFlow(t)

				const conv = "rwph-1111-2222-3333-4444"
				const cwd = "/tmp/retire-half-pair"
				f.HaveConvWithTitle(conv, "half-pair-worker")
				f.HaveAliveSession(conv, "spwn-rwph", "tmux-rwph", cwd)
				f.MarkOffline("tmux-rwph")
				f.HaveEnrolledAgent(conv)
				fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
					cwd: {Root: cwd, Branch: "feature-half", Kind: "linked"},
				})
				mux := agentd.BuildDashboardHandlerForTest()

				resp := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
					"/api/agents/"+conv+"/retire?"+row.query.Encode(), nil))
				require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
				assert.Contains(t, resp.Body.String(), "must be sent together")

				live, err := db.IsLiveAgentConv(conv)
				require.NoError(t, err)
				assert.True(t, live, "a malformed precondition must never demote the agent")
				assert.False(t, fw.wasRemoved(cwd))
				assert.Empty(t, fw.branchesRemoved())
			})
		}
	})

	t.Run("precondition without opt-in is rejected", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpu-1111-2222-3333-4444"
		const cwd = "/tmp/retire-unexpected-precondition"
		f.HaveConvWithTitle(conv, "guarded-worker")
		f.HaveAliveSession(conv, "spwn-rwpu", "tmux-rwpu", cwd)
		f.MarkOffline("tmux-rwpu")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "feature-guarded", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"shutdown":          {"1"},
			"expected_worktree": {cwd},
			"expected_branch":   {"feature-guarded"},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "require delete_worktree=1")

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.True(t, live, "an invalid precondition must never demote the agent")
		assert.False(t, fw.wasRemoved(cwd))
	})

	t.Run("empty precondition is rejected", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpe-1111-2222-3333-4444"
		const cwd = "/tmp/retire-empty-precondition"
		f.HaveConvWithTitle(conv, "empty-precondition-worker")
		f.HaveAliveSession(conv, "spwn-rwpe", "tmux-rwpe", cwd)
		f.MarkOffline("tmux-rwpe")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "feature-empty", Kind: "linked"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {""},
			"expected_branch":   {"feature-empty"},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "must not be empty")

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.True(t, live, "an empty precondition must never demote the agent")
		assert.False(t, fw.wasRemoved(cwd))
	})

	// Keep-worktree retirement and the legacy no-precondition opt-in are
	// unchanged: the guard only engages for callers that send a path.
	t.Run("established contracts survive", func(t *testing.T) {
		t.Run("keep worktree retires without touching it", func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)

			const conv = "rwpk-1111-2222-3333-4444"
			const cwd = "/tmp/retire-keep-worktree"
			f.HaveConvWithTitle(conv, "keeper-worker")
			f.HaveAliveSession(conv, "spwn-rwpk", "tmux-rwpk", cwd)
			f.MarkOffline("tmux-rwpk")
			f.HaveEnrolledAgent(conv)
			fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
				cwd: {Root: cwd, Branch: "feature-keep", Kind: "linked"},
			})
			mux := agentd.BuildDashboardHandlerForTest()

			resp := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
				"/api/agents/"+conv+"/retire?shutdown=1&delete_worktree=0", nil))
			require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())

			live, err := db.IsLiveAgentConv(conv)
			require.NoError(t, err)
			assert.False(t, live, "keep-worktree retirement still demotes")
			assert.False(t, fw.wasRemoved(cwd), "keep-worktree retirement never sweeps")
		})

		t.Run("opt-in without a precondition still deletes", func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)

			const conv = "rwpl-1111-2222-3333-4444"
			const cwd = "/tmp/retire-legacy-optin"
			f.HaveConvWithTitle(conv, "legacy-worker")
			f.HaveAliveSession(conv, "spwn-rwpl", "tmux-rwpl", cwd)
			f.MarkOffline("tmux-rwpl")
			f.HaveEnrolledAgent(conv)
			fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
				cwd: {Root: cwd, Branch: "feature-legacy", Kind: "linked"},
			})
			mux := agentd.BuildDashboardHandlerForTest()

			resp := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
				"/api/agents/"+conv+"/retire?shutdown=1&delete_worktree=1", nil))
			require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())
			assert.True(t, fw.wasRemoved(cwd), "the established delete_worktree contract is unchanged")
		})
	})

	// Request-time validation closes the dialog-to-request gap, but the common
	// live path then soft-exits the pane and removes seconds later. The world
	// keeps moving in that window: a command already running in the pane can
	// switch branches, and another agent can claim the directory. Removal is
	// --force and takes the branch with it, so the boundary itself must
	// re-confirm rather than trust the request-time snapshot.
	t.Run("removal boundary re-confirms the frozen identity", func(t *testing.T) {
		t.Run("deferred branch switch after scheduling keeps the worktree", func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)

			const conv = "rwpx-1111-2222-3333-4444"
			const cwd = "/tmp/retire-deferred-drift"
			f.HaveConvWithTitle(conv, "drifting-worker")
			f.HaveAliveSession(conv, "spwn-rwpx", "tmux-rwpx", cwd)
			f.HaveEnrolledAgent(conv)
			fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
				cwd: {Root: cwd, Branch: "feature-a", Kind: "linked"},
			})

			// A slow /exit keeps the pane alive past the response, so removal is
			// deferred to the waiter — the production shape of this race.
			cc := f.World.CCs.GetByConvID(conv)
			require.NotNil(t, cc, "no CCSim registered for %s", conv)
			cc.SetCommandDelay("/exit", 200*time.Millisecond)

			mux := agentd.BuildDashboardHandlerForTest()
			query := url.Values{
				"shutdown":          {"1"},
				"delete_worktree":   {"1"},
				"expected_worktree": {cwd},
				"expected_branch":   {"feature-a"},
			}.Encode()
			code, resp := postRetireWt(t, mux, conv, query)
			require.Equal(t, http.StatusOK, code)
			require.NotNil(t, resp.Worktree)
			require.Equal(t, "scheduled", resp.Worktree.Action,
				"a still-exiting agent must defer removal; detail=%s", resp.Worktree.Detail)
			require.False(t, fw.wasRemoved(cwd), "nothing may be removed at response time")

			// The gap: something in the still-live pane switches the checkout.
			fw.setDir(cwd, worktree.WorktreeStatus{
				Root: cwd, Branch: "feature-b", Kind: "linked",
			})
			agentd.WaitForBackgroundForTest()

			assert.False(t, fw.wasRemoved(cwd),
				"a worktree whose branch moved after confirmation must survive")
			assert.Empty(t, fw.branchesRemoved(),
				"feature-b was never confirmed and must never be force-deleted")
			msgs, err := db.ListHumanMessages()
			require.NoError(t, err)
			require.NotEmpty(t, msgs, "a deferred keep must tell the human the promise was not kept")
			assert.Contains(t, msgs[0].Subject, "kept")
			assert.Contains(t, msgs[0].Body, "feature-b")
		})

		t.Run("deferred sharer claiming the path keeps the worktree", func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)

			const conv = "rwps-1111-2222-3333-4444"
			const other = "rwpo-1111-2222-3333-4444"
			const cwd = "/tmp/retire-deferred-sharer"
			f.HaveConvWithTitle(conv, "retiring-worker")
			f.HaveAliveSession(conv, "spwn-rwps", "tmux-rwps", cwd)
			f.HaveEnrolledAgent(conv)
			fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
				cwd: {Root: cwd, Branch: "feature-a", Kind: "linked"},
			})

			cc := f.World.CCs.GetByConvID(conv)
			require.NotNil(t, cc, "no CCSim registered for %s", conv)
			cc.SetCommandDelay("/exit", 200*time.Millisecond)

			mux := agentd.BuildDashboardHandlerForTest()
			query := url.Values{
				"shutdown":          {"1"},
				"delete_worktree":   {"1"},
				"expected_worktree": {cwd},
				"expected_branch":   {"feature-a"},
			}.Encode()
			code, resp := postRetireWt(t, mux, conv, query)
			require.Equal(t, http.StatusOK, code)
			require.NotNil(t, resp.Worktree)
			require.Equal(t, "scheduled", resp.Worktree.Action)

			// The gap: another agent starts working in the same worktree.
			f.HaveConvWithTitle(other, "newcomer-worker")
			f.HaveAliveSession(other, "spwn-rwpo", "tmux-rwpo", cwd)
			f.HaveEnrolledAgent(other)
			agentd.WaitForBackgroundForTest()

			assert.False(t, fw.wasRemoved(cwd),
				"a live agent's cwd must never be removed out from under it")
			assert.Empty(t, fw.branchesRemoved())
			msgs, err := db.ListHumanMessages()
			require.NoError(t, err)
			require.NotEmpty(t, msgs)
			assert.Contains(t, msgs[0].Subject, "kept")
		})

		// The inline path (agent already offline) races other processes too. A
		// legacy caller sends no precondition, so the boundary is the ONLY
		// guard standing between a branch switch and a force-delete.
		t.Run("inline drift keeps the worktree even without a precondition", func(t *testing.T) {
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			f := newFlow(t)

			const conv = "rwpi-1111-2222-3333-4444"
			const cwd = "/tmp/retire-inline-drift"
			f.HaveConvWithTitle(conv, "inline-drift-worker")
			f.HaveAliveSession(conv, "spwn-rwpi", "tmux-rwpi", cwd)
			f.MarkOffline("tmux-rwpi")
			f.HaveEnrolledAgent(conv)

			// The checkout moves between the handler's own resolve and the
			// removal boundary: the first inspection is the request-time view,
			// every later one sees the switched branch.
			var inspects atomic.Int32
			var removedRoots, removedBranches []string
			var mu sync.Mutex
			t.Cleanup(agentd.SetWorktreeFnsForTest(
				func(dir string) worktree.WorktreeStatus {
					if dir != cwd {
						return worktree.WorktreeStatus{Kind: "none"}
					}
					branch := "feature-b"
					if inspects.Add(1) == 1 {
						branch = "feature-a"
					}
					return worktree.WorktreeStatus{Root: cwd, Branch: branch, Kind: "linked"}
				},
				func(root string, _ bool) (bool, error) {
					mu.Lock()
					removedRoots = append(removedRoots, root)
					mu.Unlock()
					return true, nil
				},
			))
			t.Cleanup(agentd.SetRetireWorktreeFnForTest(
				func(root, branch string, _ bool) (bool, bool, error) {
					mu.Lock()
					removedRoots = append(removedRoots, root)
					removedBranches = append(removedBranches, branch)
					mu.Unlock()
					return true, true, nil
				},
			))

			mux := agentd.BuildDashboardHandlerForTest()
			code, resp := postRetireWt(t, mux, conv, "shutdown=1&delete_worktree=1")
			require.Equal(t, http.StatusOK, code)
			require.NotNil(t, resp.Worktree)
			assert.Equal(t, "kept", resp.Worktree.Action,
				"drift at the boundary must report kept, never removed; detail=%s",
				resp.Worktree.Detail)
			assert.Contains(t, resp.Worktree.Detail, "feature-b")

			mu.Lock()
			defer mu.Unlock()
			assert.Empty(t, removedRoots, "a drifted worktree must not be removed")
			assert.Empty(t, removedBranches, "an unconfirmed branch must not be force-deleted")
		})
	})

	// The main worktree is never removable, so even a precondition naming it
	// exactly must be refused rather than treated as a satisfied match.
	t.Run("main worktree conflicts even when the path matches", func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		f := newFlow(t)

		const conv = "rwpn-1111-2222-3333-4444"
		const cwd = "/tmp/retire-main-repo"
		f.HaveConvWithTitle(conv, "main-repo-worker")
		f.HaveAliveSession(conv, "spwn-rwpn", "tmux-rwpn", cwd)
		f.MarkOffline("tmux-rwpn")
		f.HaveEnrolledAgent(conv)
		fw := installFakeWorktrees(t, map[string]worktree.WorktreeStatus{
			cwd: {Root: cwd, Branch: "main", Kind: "main"},
		})
		mux := agentd.BuildDashboardHandlerForTest()

		query := url.Values{
			"shutdown":          {"1"},
			"delete_worktree":   {"1"},
			"expected_worktree": {cwd},
			"expected_branch":   {"main"},
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Body.String())

		live, err := db.IsLiveAgentConv(conv)
		require.NoError(t, err)
		assert.True(t, live, "a non-removable target must not be demoted behind the conflict")
		assert.False(t, fw.wasRemoved(cwd), "the main worktree is never removable")
		assert.Empty(t, fw.branchesRemoved(), "trunk must never be swept")
	})
}
