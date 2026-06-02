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

func TestRemoveLinkedWorktreeAndBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	linkedPath, err := AddWorktreeIn(repoPath, "feature-x", "", "")
	require.NoError(t, err)

	// The retire path removes the directory AND the branch.
	removed, branchDeleted, err := RemoveLinkedWorktreeAndBranch(linkedPath, "feature-x", true)
	require.NoError(t, err)
	assert.True(t, removed, "linked worktree should be removed")
	assert.True(t, branchDeleted, "branch should be deleted")
	_, statErr := os.Stat(linkedPath)
	assert.True(t, os.IsNotExist(statErr), "worktree dir should be gone")
	assert.False(t, branchExistsIn(repoPath, "feature-x"),
		"branch must be gone after RemoveLinkedWorktreeAndBranch")
}

func TestRemoveLinkedWorktreeAndBranch_ForceDeletesUnmergedBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	linkedPath, err := AddWorktreeIn(repoPath, "feature-unmerged", "", "")
	require.NoError(t, err)

	// Commit work on the feature branch that never lands on the trunk —
	// a safe `git branch -d` would refuse this; the force path must not.
	require.NoError(t, os.WriteFile(filepath.Join(linkedPath, "new.txt"),
		[]byte("unmerged work\n"), 0o644))
	_, err = gitIn(linkedPath, "add", ".")
	require.NoError(t, err)
	_, err = gitIn(linkedPath, "commit", "-m", "unmerged commit")
	require.NoError(t, err)

	removed, branchDeleted, err := RemoveLinkedWorktreeAndBranch(linkedPath, "feature-unmerged", true)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.True(t, branchDeleted, "force delete must remove an unmerged branch")
	assert.False(t, branchExistsIn(repoPath, "feature-unmerged"))
}

func TestRemoveLinkedWorktreeAndBranch_NeverDeletesProtectedBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	linkedPath, err := AddWorktreeIn(repoPath, "feature-y", "", "")
	require.NoError(t, err)

	// Even if asked to delete a branch literally named "main"/"master",
	// the worktree goes but the protected branch is kept.
	for _, protected := range []string{"main", "master", "Main", "MASTER"} {
		removed, branchDeleted, derr := removeLinkedWorktree(linkedPath, protected, true)
		// Only the first iteration actually removes the (now-once) dir;
		// the point is the protected branch is never deleted.
		_ = removed
		require.NoError(t, derr)
		assert.False(t, branchDeleted, "%q must never be deleted", protected)
		assert.True(t, branchExistsIn(repoPath, "main"),
			"the trunk branch must survive a request to delete %q", protected)
		// Recreate the worktree for the next iteration.
		if removed {
			linkedPath, err = AddWorktreeIn(repoPath, "feature-y", "", "")
			require.NoError(t, err)
		}
	}
}

func TestRemoveLinkedWorktreeAndBranch_IdempotentOnGoneBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)
	linkedPath, err := AddWorktreeIn(repoPath, "feature-z", "", "")
	require.NoError(t, err)

	// Delete the branch out from under the call; removal should still
	// succeed for the directory and report branchDeleted=false rather
	// than erroring on the missing branch.
	removed, branchDeleted, err := RemoveLinkedWorktreeAndBranch(linkedPath, "no-such-branch", true)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.False(t, branchDeleted, "a missing branch is a silent no-op")
	assert.True(t, branchExistsIn(repoPath, "feature-z"),
		"the real branch is untouched when a wrong name is passed")
}
