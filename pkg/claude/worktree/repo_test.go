package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
