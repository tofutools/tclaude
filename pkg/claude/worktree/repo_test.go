package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoRootForPath(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	root, err := RepoRootForPath(repoPath)
	require.NoError(t, err)
	assert.Equal(t, normalizePath(repoPath), normalizePath(root))

	// A subdirectory still resolves up to the repo root.
	sub := filepath.Join(repoPath, "nested", "deeper")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	root, err = RepoRootForPath(sub)
	require.NoError(t, err)
	assert.Equal(t, normalizePath(repoPath), normalizePath(root))

	// A directory outside any git repo errors.
	_, err = RepoRootForPath(t.TempDir())
	assert.Error(t, err)

	// A path that doesn't exist at all errors.
	_, err = RepoRootForPath(filepath.Join(repoPath, "no-such-dir"))
	assert.Error(t, err)
}

func TestListWorktreesIn(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	wts, err := ListWorktreesIn(repoPath)
	require.NoError(t, err)
	require.Len(t, wts, 1)
	assert.True(t, wts[0].IsMain)
	assert.Equal(t, "main", wts[0].Branch)
}

func TestParsePrunableWorktrees(t *testing.T) {
	out := parsePrunableWorktrees(`Removing worktrees/topic-one: gitdir file does not exist
Removing worktrees/topic-two: gitdir file points to non-existent location
warning: unrelated output
Removing malformed-entry`)

	assert.Equal(t, []PrunableWorktree{
		{AdminDir: "topic-one", Reason: "gitdir file does not exist"},
		{AdminDir: "topic-two", Reason: "gitdir file points to non-existent location"},
	}, out)
}

func TestParseWorktreePorcelainKeepsPruneAndLockState(t *testing.T) {
	wts := parseWorktreePorcelain(`worktree /repo
HEAD abc
branch refs/heads/main

worktree /repo-missing
HEAD def
branch refs/heads/topic
prunable gitdir file points to non-existent location
locked operator hold
`)
	require.Len(t, wts, 2)
	assert.True(t, wts[0].IsMain)
	assert.True(t, wts[1].Prunable)
	assert.Equal(t, "gitdir file points to non-existent location", wts[1].PrunableReason)
	assert.True(t, wts[1].Locked)
	assert.Equal(t, "operator hold", wts[1].LockReason)
}

func TestPrunableWorktreesInReadsGitVerboseStream(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)
	require.NoError(t, exec.Command("git", "-C", repoPath, "config",
		"gc.worktreePruneExpire", "now").Run())
	healthyPath := filepath.Join(parentDir, "healthy-listed")
	require.NoError(t, exec.Command("git", "-C", repoPath, "worktree", "add",
		"-b", "healthy-listed", healthyPath).Run())
	listedPath := filepath.Join(parentDir, "listed-prunable")
	require.NoError(t, exec.Command("git", "-C", repoPath, "worktree", "add",
		"-b", "listed-prunable", listedPath).Run())
	require.NoError(t, os.Rename(listedPath, listedPath+"-gone"),
		"delete the checkout out-of-band while retaining its readable gitdir registration")
	require.NoError(t, exec.Command("git", "-C", repoPath, "worktree", "lock", "--reason",
		tclaudePruneProtectionReason, listedPath).Run(),
		"simulate a tclaude-owned protection lock abandoned by an interrupted prune")

	adminDir := filepath.Join(repoPath, ".git", "worktrees", "stale-test")
	require.NoError(t, os.MkdirAll(adminDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(adminDir, "HEAD"), []byte("ref: refs/heads/stale\n"), 0o644))
	old := time.Now().Add(-365 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(adminDir, old, old))
	require.NoError(t, os.Chtimes(filepath.Join(adminDir, "HEAD"), old, old))

	entries, err := PrunableWorktreesIn(repoPath)
	require.NoError(t, err)
	require.Len(t, entries, 1,
		"the porcelain-visible prunable worktree stays in the individual checkout flow")
	assert.Equal(t, "stale-test", entries[0].AdminDir)
	assert.Contains(t, entries[0].Reason, "gitdir file")
	listedAfterRecovery, err := ListWorktreesIn(repoPath)
	require.NoError(t, err)
	for _, wt := range listedAfterRecovery {
		if filepath.Clean(wt.Path) == filepath.Clean(listedPath) {
			assert.False(t, wt.Locked, "a later preview recovers an abandoned tclaude-owned lock")
		}
	}

	stderr, err := PruneWorktreesIn(repoPath)
	require.NoError(t, err, "repo prune should temporarily protect the listed worktree: %s", stderr)
	remaining, err := PrunableWorktreesIn(repoPath)
	require.NoError(t, err)
	assert.Empty(t, remaining, "the invisible administrative entry was pruned")
	listed, err := ListWorktreesIn(repoPath)
	require.NoError(t, err)
	var protected *WorktreeInfo
	for i := range listed {
		if filepath.Clean(listed[i].Path) == filepath.Clean(listedPath) {
			protected = &listed[i]
		}
	}
	require.NotNil(t, protected, "repo prune must not bypass the individually managed row")
	assert.True(t, protected.Prunable)
	assert.False(t, protected.Locked, "tclaude's temporary protection lock is restored")
	healthyFound := false
	for _, wt := range listed {
		if filepath.Clean(wt.Path) == filepath.Clean(healthyPath) {
			healthyFound = true
			assert.False(t, wt.Locked, "healthy listed rows are protected across the prune race, then restored")
		}
	}
	assert.True(t, healthyFound)
}

func TestAddWorktreeIn(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	// New branch, default path: ../<repo>-<branch>.
	path, err := AddWorktreeIn(repoPath, "feature-x", "", "")
	require.NoError(t, err)
	want := filepath.Join(parentDir, filepath.Base(repoPath)+"-feature-x")
	assert.Equal(t, normalizePath(want), normalizePath(path))
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())

	// The new worktree shows up when listing from the original repo.
	wts, err := ListWorktreesIn(repoPath)
	require.NoError(t, err)
	assert.Len(t, wts, 2)

	// Re-adding the same branch fails — its path already exists.
	_, err = AddWorktreeIn(repoPath, "feature-x", "", "")
	assert.Error(t, err)

	// An empty branch name is rejected.
	_, err = AddWorktreeIn(repoPath, "", "", "")
	assert.Error(t, err)
}

func TestAddWorktreeInExplicitPath(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	custom := filepath.Join(parentDir, "custom-worktree")
	path, err := AddWorktreeIn(repoPath, "feature-y", "main", custom)
	require.NoError(t, err)
	assert.Equal(t, normalizePath(custom), normalizePath(path))
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
}

func TestAddWorktreeInWithoutTrackingIgnoresLockedConfig(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)
	require.NoError(t, exec.Command("git", "-C", repoPath, "remote", "add", "origin", repoPath).Run())
	require.NoError(t, exec.Command("git", "-C", repoPath, "update-ref",
		"refs/remotes/origin/main", "HEAD").Run())
	lockPath := filepath.Join(repoPath, ".git", "config.lock")
	require.NoError(t, os.WriteFile(lockPath, nil, 0o644))
	t.Cleanup(func() { _ = os.Remove(lockPath) })

	path, err := AddWorktreeInWithoutTracking(repoPath, "fallback", "refs/remotes/origin/main", "")
	require.NoError(t, err)
	assert.Equal(t, normalizePath(filepath.Join(parentDir, "test-repo-fallback")), normalizePath(path))
	_, err = gitIn(repoPath, "rev-parse", "--abbrev-ref", "fallback@{upstream}")
	assert.Error(t, err, "the fallback must not claim origin/main as its upstream")
}

func TestConfigLockErrorClassification(t *testing.T) {
	lockErr := fmt.Errorf("failed to create worktree: git worktree add: error: could not lock config file .git/config: File exists\nerror: unable to write upstream branch configuration")
	assert.True(t, IsConfigLockError(lockErr))
	assert.True(t, IsUpstreamConfigLockError(lockErr))
	assert.False(t, IsUpstreamConfigLockError(fmt.Errorf("could not lock config file .git/config: Permission denied")))
}

func TestRemoveSandboxConfigLockInPreservesOtherLockShapes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		perm os.FileMode
	}{
		{name: "writable empty Git lock", perm: 0o644},
		{name: "read-only nonempty file", data: []byte("config in progress"), perm: 0o444},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath, _ := setupTestRepo(t)
			lockPath := filepath.Join(repoPath, ".git", "config.lock")
			require.NoError(t, os.WriteFile(lockPath, tt.data, tt.perm))

			removed, err := RemoveSandboxConfigLockIn(repoPath)
			require.NoError(t, err)
			assert.False(t, removed)
			assert.FileExists(t, lockPath)
		})
	}
}

func TestFindSubRepos(t *testing.T) {
	// A "virtual monorepo": a plain dir holding docs plus nested git
	// repos at varying depths — exactly the layout RepoRootForPath
	// fails on and FindSubRepos is meant to rescue.
	mono := t.TempDir()
	mono, err := filepath.EvalSymlinks(mono)
	require.NoError(t, err, "resolve symlinks")
	require.NoError(t, os.WriteFile(filepath.Join(mono, "CLAUDE.md"), []byte("# docs\n"), 0o644))

	mkRepo := func(rel string) {
		p := filepath.Join(mono, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(p, 0o755), "mkdir %s", rel)
		require.NoErrorf(t, exec.Command("git", "-C", p, "init").Run(), "git init %s", rel)
	}
	mkRepo("actual-repo")               // depth 1
	mkRepo("some-category/nested-repo") // depth 2
	mkRepo("a/b/c/deep-repo")           // depth 4
	mkRepo("node_modules/skipme")       // inside a skipped dir

	toSlash := func(subs []SubRepo) []string {
		rels := make([]string, len(subs))
		for i, s := range subs {
			assert.Truef(t, filepath.IsAbs(s.Path), "Path should be absolute: %s", s.Path)
			rels[i] = filepath.ToSlash(s.Rel)
		}
		return rels
	}

	subs := FindSubRepos(mono, 4)
	rels := toSlash(subs)
	assert.Contains(t, rels, "actual-repo")
	assert.Contains(t, rels, "some-category/nested-repo")
	assert.Contains(t, rels, "a/b/c/deep-repo")
	assert.NotContains(t, rels, "node_modules/skipme", "node_modules must be skipped")

	// Result is sorted by Rel (native separator).
	native := make([]string, len(subs))
	for i, s := range subs {
		native[i] = s.Rel
	}
	assert.True(t, sort.StringsAreSorted(native), "result should be sorted by Rel: %v", native)

	// maxDepth caps recursion — the depth-4 repo is invisible at 2.
	shallow := toSlash(FindSubRepos(mono, 2))
	assert.Contains(t, shallow, "actual-repo")
	assert.Contains(t, shallow, "some-category/nested-repo")
	assert.NotContains(t, shallow, "a/b/c/deep-repo", "maxDepth should cap recursion")

	// A repo is a leaf: a repo nested inside another repo is never
	// returned, because the walk stops descending at the outer repo.
	mkRepo("actual-repo/inner")
	for _, r := range toSlash(FindSubRepos(mono, 4)) {
		assert.NotEqual(t, "actual-repo/inner", r, "must not descend into a repo")
	}

	// Degenerate inputs yield nothing rather than panicking.
	assert.Nil(t, FindSubRepos("", 4))
	assert.Nil(t, FindSubRepos(mono, 0))
}

// setupEmptyTestRepo creates a `git init`-ed repo with NO commits —
// an unborn HEAD on `main`. Returns the repo path and its parent dir
// (where default-located worktrees land).
func setupEmptyTestRepo(t *testing.T) (repoPath string, parentDir string) {
	t.Helper()
	parentDir = t.TempDir()
	parentDir, err := filepath.EvalSymlinks(parentDir)
	require.NoError(t, err, "resolve symlinks")
	repoPath = filepath.Join(parentDir, "empty-repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	require.NoError(t, exec.Command("git", "-C", repoPath, "init", "-b", "main").Run(), "git init")
	_ = exec.Command("git", "-C", repoPath, "config", "user.email", "test@example.com").Run()
	_ = exec.Command("git", "-C", repoPath, "config", "user.name", "Test User").Run()
	return repoPath, parentDir
}

func TestHasCommitsIn(t *testing.T) {
	withCommits, _ := setupTestRepo(t)
	assert.True(t, HasCommitsIn(withCommits), "repo with an initial commit has commits")

	empty, _ := setupEmptyTestRepo(t)
	assert.False(t, HasCommitsIn(empty), "freshly init'd repo (unborn HEAD) has no commits")
}

// TestAddWorktreeInEmptyRepo is the regression for spawning a worktree
// into a brand-new repo with no commits yet: `git worktree add … <base>`
// can't work (no commit to base on), so AddWorktreeIn cuts an orphan
// branch instead. Before the fix the picker showed an empty base list
// and creation failed with "could not determine base branch".
func TestAddWorktreeInEmptyRepo(t *testing.T) {
	repoPath, parentDir := setupEmptyTestRepo(t)

	// No base branch is available (nor needed) — the worktree is cut as
	// an orphan branch. An empty from_branch is the dashboard's default.
	path, err := AddWorktreeIn(repoPath, "feature-x", "", "")
	require.NoError(t, err, "orphan worktree should succeed in a no-commit repo")
	want := filepath.Join(parentDir, "empty-repo-feature-x")
	assert.Equal(t, normalizePath(want), normalizePath(path))
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())

	// The orphan worktree is checked out on its own branch and shows up
	// alongside the main worktree.
	wts, err := ListWorktreesIn(repoPath)
	require.NoError(t, err)
	branches := map[string]bool{}
	for _, wt := range wts {
		branches[wt.Branch] = true
	}
	assert.Truef(t, branches["feature-x"], "feature-x worktree should be listed, got %v", branches)
}

func TestBranchesAndDefaultBranchIn(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	for _, b := range []string{"feature-a", "feature-b"} {
		cmd := exec.Command("git", "-C", repoPath, "branch", b)
		require.NoErrorf(t, cmd.Run(), "create branch %s", b)
	}

	branches := BranchesIn(repoPath)
	for _, want := range []string{"main", "feature-a", "feature-b"} {
		assert.Containsf(t, branches, want, "BranchesIn should include %s", want)
	}

	def, err := DefaultBranchIn(repoPath)
	require.NoError(t, err)
	assert.Equal(t, "main", def)
}

func TestFetchTargetForBranchInRecognizesRemoteQualifiedPickerBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	require.NoError(t, exec.Command("git", "-C", repoPath, "remote", "add", "upstream", repoPath).Run())
	require.NoError(t, exec.Command("git", "-C", repoPath, "update-ref",
		"refs/remotes/upstream/topic", "HEAD").Run())

	remote, branch, tracking, err := FetchTargetForBranchIn(repoPath, "upstream/topic")
	require.NoError(t, err)
	assert.Equal(t, "upstream", remote)
	assert.Equal(t, "topic", branch)
	assert.Equal(t, "refs/remotes/upstream/topic", tracking)
}
