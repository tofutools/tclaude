package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectWorktree(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	// The repo's primary checkout classifies as "main".
	main := InspectWorktree(repoPath)
	assert.Equal(t, "main", main.Kind)
	assert.Equal(t, normalizePath(repoPath), normalizePath(main.Root))
	assert.Equal(t, "main", main.Branch)

	// A `git worktree add` checkout classifies as "linked".
	linkedPath, err := AddWorktreeIn(repoPath, "feature-x", "", "")
	require.NoError(t, err)
	linked := InspectWorktree(linkedPath)
	assert.Equal(t, "linked", linked.Kind)
	assert.Equal(t, normalizePath(linkedPath), normalizePath(linked.Root))
	assert.Equal(t, "feature-x", linked.Branch)

	// A subdir of the linked worktree still resolves to its root.
	sub := filepath.Join(linkedPath, "nested")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	assert.Equal(t, "linked", InspectWorktree(sub).Kind)

	// A path outside any git repo is "none".
	assert.Equal(t, "none", InspectWorktree(t.TempDir()).Kind)
	assert.Equal(t, "none", InspectWorktree("").Kind)
	assert.Equal(t, "none", InspectWorktree(filepath.Join(repoPath, "no-such")).Kind)
}

func TestRemoveLinkedWorktree(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	linkedPath, err := AddWorktreeIn(repoPath, "feature-x", "", "")
	require.NoError(t, err)

	// First removal succeeds and the directory is gone.
	removed, err := RemoveLinkedWorktree(linkedPath, true)
	require.NoError(t, err)
	assert.True(t, removed, "linked worktree should be removed")
	_, statErr := os.Stat(linkedPath)
	assert.True(t, os.IsNotExist(statErr), "worktree dir should be gone")

	// The branch survives — only the working directory went.
	assert.True(t, branchExistsIn(repoPath, "feature-x"), "branch must outlive its worktree")

	// Re-removing is a silent no-op (idempotent).
	removed, err = RemoveLinkedWorktree(linkedPath, true)
	require.NoError(t, err)
	assert.False(t, removed, "second removal is a no-op")

	// An empty path is a no-op, not an error.
	removed, err = RemoveLinkedWorktree("", true)
	require.NoError(t, err)
	assert.False(t, removed)
}

func TestRemoveLinkedWorktree_RefusesMain(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	removed, err := RemoveLinkedWorktree(repoPath, true)
	assert.Error(t, err, "removing the main worktree must be refused")
	assert.False(t, removed)
	_, statErr := os.Stat(repoPath)
	assert.NoError(t, statErr, "main repo dir must be untouched")
}

func TestRemoveLinkedWorktree_ForceClearsDirtyTree(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	linkedPath, err := AddWorktreeIn(repoPath, "feature-dirty", "", "")
	require.NoError(t, err)

	// Leave both an untracked file and an uncommitted edit — a
	// non-force `git worktree remove` would refuse both.
	require.NoError(t, os.WriteFile(filepath.Join(linkedPath, "scratch.txt"),
		[]byte("untracked\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(linkedPath, "README.md"),
		[]byte("# edited\n"), 0o644))

	removed, err := RemoveLinkedWorktree(linkedPath, true)
	require.NoError(t, err, "force removal must clear a dirty worktree")
	assert.True(t, removed)
}
