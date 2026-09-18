package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

func TestAWBReadyCleanupRetiresSettledAgentAfterClose(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	installEmptyTmuxForTest(t)
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	a, err := db.GetAgent(agentID)
	require.NoError(t, err)
	_, err = db.PromoteAgent(a.CurrentConvID, "promote")
	require.NoError(t, err)
	testAWBReadySpawnedDispatch(t, agentID)

	server := httptest.NewServer(http.HandlerFunc(testAWBReadyMergedPRIssue(t, nil)))
	t.Cleanup(server.Close)
	testAWBReadyGitHub(t, "MERGED")

	require.NoError(t, testAWBReadyMonitorWorker(t, server.URL).tick(context.Background()))

	state, err := db.AgentState(a.CurrentConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state,
		"an automatic closure retires the actor it spawned for the issue")
}

func TestAWBReadyCleanupLeavesAgentThatWentBackToWork(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	installEmptyTmuxForTest(t)
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	a, err := db.GetAgent(agentID)
	require.NoError(t, err)
	_, err = db.PromoteAgent(a.CurrentConvID, "promote")
	require.NoError(t, err)
	testAWBReadySpawnedDispatch(t, agentID)

	// The settle check that authorised the closure is already one AWB call old
	// by the time cleanup runs. Flip the pane back to working inside the close
	// request to prove the sweep re-asks rather than trusting that verdict.
	server := httptest.NewServer(http.HandlerFunc(testAWBReadyMergedPRIssue(t, func() {
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID: "session-" + a.CurrentConvID, ConvID: a.CurrentConvID, Status: session.StatusWorking}))
	})))
	t.Cleanup(server.Close)
	testAWBReadyGitHub(t, "MERGED")

	require.NoError(t, testAWBReadyMonitorWorker(t, server.URL).tick(context.Background()))

	state, err := db.AgentState(a.CurrentConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state,
		"an actor that picked its work back up must not be retired under itself")
}

func TestAWBReadyCleanupRemovesWorktreeAndMergedBranch(t *testing.T) {
	removed := testAWBReadyWorktreeCleanup(t, true)
	require.Len(t, removed, 1)
	assert.Equal(t, "tcl-a1", removed[0].branch,
		"a branch the sweep proved merged is deleted with its worktree")
}

func TestAWBReadyCleanupKeepsUnmergedBranch(t *testing.T) {
	removed := testAWBReadyWorktreeCleanup(t, false)
	require.Len(t, removed, 1)
	assert.Empty(t, removed[0].branch,
		"an unproven branch keeps its commits; only the working directory goes")
}

type awbReadyRemovedWorktree struct{ root, branch string }

// testAWBReadyWorktreeCleanup drives one worktree-mode pickup through its
// automatic closure and reports what the retire-time git seam was asked to
// remove.
func testAWBReadyWorktreeCleanup(t *testing.T, merged bool) []awbReadyRemovedWorktree {
	t.Helper()
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	installEmptyTmuxForTest(t)

	wtPath := t.TempDir()
	const convID = "conv-awb-worktree"
	agentID, err := db.AllocateAgent(convID, "spawn")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "session-" + convID, ConvID: convID, Cwd: wtPath, Status: session.StatusIdle}))
	_, err = db.PromoteAgent(convID, "promote")
	require.NoError(t, err)
	testAWBReadySpawnedDispatch(t, agentID)

	t.Cleanup(SetWorktreeFnsForTest(
		func(dir string) worktree.WorktreeStatus {
			if filepath.Clean(dir) != filepath.Clean(wtPath) {
				return worktree.WorktreeStatus{Kind: "none"}
			}
			return worktree.WorktreeStatus{Root: wtPath, Branch: "tcl-a1", Kind: "linked"}
		},
		func(string, bool) (bool, error) { return true, nil },
	))
	var seen []awbReadyRemovedWorktree
	t.Cleanup(SetRetireWorktreeFnForTest(func(root, branch string, _ bool) (bool, bool, error) {
		seen = append(seen, awbReadyRemovedWorktree{root: root, branch: branch})
		return true, branch != "", nil
	}))
	previousMerged := liveAWBReadyBranchMergedFn
	liveAWBReadyBranchMergedFn = func(_ context.Context, _, branch string) (bool, error) {
		assert.Equal(t, "tcl-a1", branch)
		return merged, nil
	}
	t.Cleanup(func() { liveAWBReadyBranchMergedFn = previousMerged })

	server := httptest.NewServer(http.HandlerFunc(testAWBReadyMergedPRIssue(t, nil)))
	t.Cleanup(server.Close)
	testAWBReadyGitHub(t, "MERGED")

	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.Worktree = true
	require.NoError(t, worker.tick(context.Background()))

	state, err := db.AgentState(convID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state)
	return seen
}

func TestAWBReadyBranchMergedAcceptsMergedPullRequestHead(t *testing.T) {
	setupTestDB(t)
	previous := liveAWBReadyBranchMergedFn
	liveAWBReadyBranchMergedFn = func(context.Context, string, string) (bool, error) {
		t.Error("a merged pull request head needs no local ancestry check")
		return false, nil
	}
	t.Cleanup(func() { liveAWBReadyBranchMergedFn = previous })

	w := awbReadyWorker{process: "builders", workspace: "tcl"}
	merged, why := w.branchMerged(context.Background(), "tcl-a1",
		awbReadyPRState{Merged: true, HeadRef: "tcl-a1", BaseRef: "main"})
	assert.True(t, merged)
	assert.Contains(t, why, "merged pull request")
}

func TestAWBReadyBranchMergedRejectsPullRequestForAnotherBranch(t *testing.T) {
	setupTestDB(t)
	previous := liveAWBReadyBranchMergedFn
	var asked int
	liveAWBReadyBranchMergedFn = func(context.Context, string, string) (bool, error) {
		asked++
		return false, nil
	}
	t.Cleanup(func() { liveAWBReadyBranchMergedFn = previous })

	w := awbReadyWorker{process: "builders", workspace: "tcl"}
	merged, _ := w.branchMerged(context.Background(), "tcl-a1",
		awbReadyPRState{Merged: true, HeadRef: "tcl-other", BaseRef: "main"})
	assert.False(t, merged)
	assert.Equal(t, 1, asked, "a verdict about another branch falls back to local ancestry")

	merged, _ = w.branchMerged(context.Background(), "tcl-a1",
		awbReadyPRState{Merged: true, HeadRef: "tcl-a1", BaseRef: "release/1.2"})
	assert.False(t, merged, "a pull request merged somewhere other than a trunk proves nothing about main")
}

func TestAWBReadyBranchMergedKeepsDetachedHead(t *testing.T) {
	setupTestDB(t)
	w := awbReadyWorker{process: "builders", workspace: "tcl"}
	merged, why := w.branchMerged(context.Background(), "", awbReadyPRState{Merged: true})
	assert.False(t, merged)
	assert.Contains(t, why, "detached HEAD")
}

func TestAWBReadyBranchMergedOnLocalMain(t *testing.T) {
	setupTestDB(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	repo := filepath.Join(t.TempDir(), "repo")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	require.NoError(t, exec.Command("git", "init", "-b", "main", repo).Run())
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "tclaude test")
	git("config", "commit.gpgsign", "false")
	git("commit", "--allow-empty", "-m", "on main")
	git("switch", "-c", "tcl-a1")
	git("commit", "--allow-empty", "-m", "feature")

	merged, err := liveAWBReadyBranchMerged(context.Background(), repo, "tcl-a1")
	require.NoError(t, err)
	assert.False(t, merged, "an unmerged feature branch is not eligible for deletion")

	git("switch", "main")
	git("merge", "--ff-only", "tcl-a1")
	merged, err = liveAWBReadyBranchMerged(context.Background(), repo, "tcl-a1")
	require.NoError(t, err)
	assert.True(t, merged)

	merged, err = liveAWBReadyBranchMerged(context.Background(), repo, "tcl-never-existed")
	require.NoError(t, err)
	assert.False(t, merged, "a branch that is already gone is not an error, just nothing to delete")

	_, err = liveAWBReadyBranchMerged(context.Background(), repo, "--upload-pack=evil")
	assert.ErrorContains(t, err, "invalid branch name")
}

func TestApplyRetireWorktreeCleanupKeepsBranchWhenAsked(t *testing.T) {
	setupTestDB(t)
	var gotBranch string
	t.Cleanup(SetRetireWorktreeFnForTest(func(_, branch string, _ bool) (bool, bool, error) {
		gotBranch = branch
		return true, branch != "", nil
	}))
	note, ok := applyRetireWorktreeCleanup(agentWorktreeView{
		Path: "/tmp/wt", Branch: "tcl-a1", Kind: "linked", KeepBranch: true}, true)
	assert.True(t, ok)
	assert.Empty(t, gotBranch)
	assert.Equal(t, "worktree removed (branch tcl-a1 kept)", note)
}

// testAWBReadySpawnedDispatch puts the polling process in the state the monitor
// arm of tick() acts on: one issue, already spawned onto agentID.
func testAWBReadySpawnedDispatch(t *testing.T, agentID string) {
	t.Helper()
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)
}

// testAWBReadyMergedPRIssue serves an issue carrying a pull request URL and
// accepts its closure. onClose, when set, runs inside the close request — the
// window between the settle check that authorised the closure and the sweep
// that follows it.
func testAWBReadyMergedPRIssue(t *testing.T, onClose func()) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress",
			PullRequestURL: "https://github.com/acme/repo/pull/42"}
		if r.Method == http.MethodPost && r.URL.Path == "/api/issues/tcl-a1/close" {
			if onClose != nil {
				onClose()
			}
			issue.Status = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(issue))
	}
}

// installEmptyTmuxForTest reports "no panes are running" from the tmux seam, so
// the worktree claim snapshot completes rather than failing closed on a host
// without a tmux server.
func installEmptyTmuxForTest(t *testing.T) {
	t.Helper()
	previous := clcommon.Default
	clcommon.Default = &commandRecordingTmux{}
	t.Cleanup(func() { clcommon.Default = previous })
}
