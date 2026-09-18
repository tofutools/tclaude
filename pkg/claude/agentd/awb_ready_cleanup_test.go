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
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", removed[0].expectTip,
		"the delete is conditional on the commit the proof was about")
}

func TestAWBReadyCleanupKeepsUnmergedBranch(t *testing.T) {
	removed := testAWBReadyWorktreeCleanup(t, false)
	require.Len(t, removed, 1)
	assert.Empty(t, removed[0].branch,
		"an unproven branch keeps its commits; only the working directory goes")
}

type awbReadyRemovedWorktree struct{ root, branch, expectTip string }

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
	t.Cleanup(SetRetireWorktreeAtFnForTest(
		func(_, root, branch, expectTip string, _ bool) (bool, bool, string, error) {
			seen = append(seen, awbReadyRemovedWorktree{root: root, branch: branch, expectTip: expectTip})
			return true, true, branch, nil
		}))
	const tip = "0123456789abcdef0123456789abcdef01234567"
	previousTip := awbReadyBranchTipFn
	awbReadyBranchTipFn = func(_ context.Context, _, branch string) (string, error) {
		assert.Equal(t, "tcl-a1", branch)
		return tip, nil
	}
	t.Cleanup(func() { awbReadyBranchTipFn = previousTip })
	previousMain := liveAWBReadyCommitOnMainFn
	liveAWBReadyCommitOnMainFn = func(_ context.Context, _, commit string) (bool, bool, error) {
		assert.Equal(t, tip, commit)
		return merged, true, nil
	}
	t.Cleanup(func() { liveAWBReadyCommitOnMainFn = previousMain })

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
	const tip = "0123456789abcdef0123456789abcdef01234567"
	installAWBReadyTipForTest(t, tip)
	previous := liveAWBReadyCommitOnMainFn
	liveAWBReadyCommitOnMainFn = func(context.Context, string, string) (bool, bool, error) {
		t.Error("a pull request that merged this exact commit needs no ancestry check")
		return false, false, nil
	}
	t.Cleanup(func() { liveAWBReadyCommitOnMainFn = previous })

	w := awbReadyWorker{process: "builders", workspace: "tcl"}
	at, why := w.branchMergedAt(context.Background(), "tcl-a1",
		awbReadyPRState{Merged: true, HeadRef: "tcl-a1", HeadOID: tip, BaseRef: "main"})
	assert.Equal(t, tip, at, "the licence to delete is pinned to the proven commit")
	assert.Contains(t, why, "merged pull request")
}

// A pull-request verdict only covers the branch and commit it names. Each of
// these near-misses must fall through to the local ancestry check rather than
// stand in for it — the fallthrough is what keeps a branch that moved on since
// the merge, or merged somewhere other than a trunk, from being deleted.
func TestAWBReadyBranchMergedRejectsPullRequestThatCoversSomethingElse(t *testing.T) {
	setupTestDB(t)
	const tip = "0123456789abcdef0123456789abcdef01234567"
	installAWBReadyTipForTest(t, tip)
	var asked int
	previous := liveAWBReadyCommitOnMainFn
	liveAWBReadyCommitOnMainFn = func(context.Context, string, string) (bool, bool, error) {
		asked++
		return false, true, nil
	}
	t.Cleanup(func() { liveAWBReadyCommitOnMainFn = previous })

	w := awbReadyWorker{process: "builders", workspace: "tcl"}
	for name, pr := range map[string]awbReadyPRState{
		"another branch": {Merged: true, HeadRef: "tcl-other", HeadOID: tip, BaseRef: "main"},
		"an older commit": {Merged: true, HeadRef: "tcl-a1", BaseRef: "main",
			HeadOID: "99999999999999999999999999999999999999ff"},
		"a non-trunk base": {Merged: true, HeadRef: "tcl-a1", HeadOID: tip, BaseRef: "release/1.2"},
	} {
		at, _ := w.branchMergedAt(context.Background(), "tcl-a1", pr)
		assert.Empty(t, at, "a verdict about %s proves nothing about this branch tip", name)
	}
	assert.Equal(t, 3, asked, "every near-miss falls back to local ancestry")
}

func TestAWBReadyBranchMergedKeepsUnresolvableBranch(t *testing.T) {
	setupTestDB(t)
	w := awbReadyWorker{process: "builders", workspace: "tcl"}
	at, why := w.branchMergedAt(context.Background(), "", awbReadyPRState{Merged: true})
	assert.Empty(t, at)
	assert.Contains(t, why, "detached HEAD")

	installAWBReadyTipForTest(t, "")
	at, why = w.branchMergedAt(context.Background(), "tcl-a1",
		awbReadyPRState{Merged: true, HeadRef: "tcl-a1", BaseRef: "main"})
	assert.Empty(t, at)
	assert.Contains(t, why, "no longer exists")
}

func installAWBReadyTipForTest(t *testing.T, tip string) {
	t.Helper()
	previous := awbReadyBranchTipFn
	awbReadyBranchTipFn = func(context.Context, string, string) (string, error) { return tip, nil }
	t.Cleanup(func() { awbReadyBranchTipFn = previous })
}

func TestAWBReadyBranchTipResolvesLocalBranches(t *testing.T) {
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
	want := git("rev-parse", "refs/heads/tcl-a1")

	tip, err := awbReadyBranchTip(context.Background(), repo, "tcl-a1")
	require.NoError(t, err)
	assert.Equal(t, want, tip)

	tip, err = awbReadyBranchTip(context.Background(), repo, "tcl-never-existed")
	require.NoError(t, err)
	assert.Empty(t, tip, "a branch that is already gone is not an error, just nothing to delete")

	_, err = awbReadyBranchTip(context.Background(), repo, "--upload-pack=evil")
	assert.ErrorContains(t, err, "invalid branch name")
}

// End-to-end over real git: an unmerged branch is kept, the same branch is
// deletable once main contains it.
func TestAWBReadyBranchMergedFollowsLocalAncestry(t *testing.T) {
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

	w := awbReadyWorker{process: "builders", workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: repo}}
	at, why := w.branchMergedAt(context.Background(), "tcl-a1", awbReadyPRState{})
	assert.Empty(t, at, "an unmerged feature branch is not eligible for deletion: %s", why)

	git("switch", "main")
	git("merge", "--ff-only", "tcl-a1")
	at, _ = w.branchMergedAt(context.Background(), "tcl-a1", awbReadyPRState{})
	assert.Equal(t, git("rev-parse", "refs/heads/tcl-a1"), at)
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

// The registration-anchored removal is a distinct boolean API from the
// path-anchored one, and it is the safety-sensitive half: it re-reads the branch
// from git rather than taking the caller's. Both halves of the branch policy
// therefore need their own assertion on it.
func TestApplyRetireWorktreeCleanupOnRegisteredPath(t *testing.T) {
	setupTestDB(t)
	const tip = "0123456789abcdef0123456789abcdef01234567"
	var unconditionalDelete *bool
	t.Cleanup(SetRegisteredWorktreeFnForTest(
		func(_, _ string, deleteBranch, _ bool) (bool, bool, string, error) {
			unconditionalDelete = &deleteBranch
			return true, deleteBranch, "tcl-a1", nil
		}))
	var conditional struct{ anchor, branch, tip string }
	t.Cleanup(SetRetireWorktreeAtFnForTest(
		func(anchorPath, _, branch, expectTip string, _ bool) (bool, bool, string, error) {
			conditional.anchor, conditional.branch, conditional.tip = anchorPath, branch, expectTip
			return true, true, branch, nil
		}))

	base := agentWorktreeView{Path: "/tmp/wt", Branch: "tcl-a1", Kind: "linked", RepoRoot: "/tmp/repo"}

	keep := base
	keep.KeepBranch = true
	note, ok := applyRetireWorktreeCleanup(keep, true)
	assert.True(t, ok)
	require.NotNil(t, unconditionalDelete)
	assert.False(t, *unconditionalDelete, "an unproven branch is kept on the registered path too")
	assert.Equal(t, "worktree removed (branch tcl-a1 kept)", note)

	proven := base
	proven.BranchTip = tip
	note, ok = applyRetireWorktreeCleanup(proven, true)
	assert.True(t, ok)
	assert.Equal(t, "/tmp/repo", conditional.anchor,
		"the registered path's anchor must reach the conditional removal")
	assert.Equal(t, "tcl-a1", conditional.branch)
	assert.Equal(t, tip, conditional.tip)
	assert.Equal(t, "worktree + branch tcl-a1 removed", note)
}

// The guard is what makes the retire fail closed: a turn that starts after the
// sweep's own settle check but before the demotion commits must leave the actor
// enrolled. Driving retireAgentConvGuarded directly is the only way to sit in
// that window.
func TestAWBReadyRetireGuardAbortsOnAnAgentThatWentBackToWork(t *testing.T) {
	setupTestDB(t)
	installEmptyTmuxForTest(t)
	const convID = "conv-awb-guard"
	agentID, err := db.AllocateAgent(convID, "spawn")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "session-" + convID, ConvID: convID, Status: session.StatusIdle}))
	_, err = db.PromoteAgent(convID, "promote")
	require.NoError(t, err)

	require.NoError(t, awbReadyStillSettled(agentID), "an idle actor passes the guard")

	_, _, err = retireAgentConvGuarded(convID, awbReadyRetireActor, "closed automatically", false,
		func() error {
			// Stand in for the human who started a turn while the sweep was
			// resolving worktrees and fetching main.
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "session-" + convID, ConvID: convID, Status: session.StatusWorking}))
			return awbReadyStillSettled(agentID)
		})
	assert.ErrorIs(t, err, errAWBReadyAgentBusy)

	state, err := db.AgentState(convID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "an aborted guard must change nothing")
}
