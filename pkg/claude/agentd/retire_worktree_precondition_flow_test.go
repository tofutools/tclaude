package agentd_test

import (
	"net/http"
	"net/url"
	"testing"

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
		}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusConflict, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "worktree changed")
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

		query := url.Values{"shutdown": {"1"}, "expected_worktree": {cwd}}.Encode()
		resp := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/retire?"+query, nil))
		require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
		assert.Contains(t, resp.Body.String(), "requires delete_worktree=1")

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
