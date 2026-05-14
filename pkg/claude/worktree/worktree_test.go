package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestRepo creates a temporary git repository for testing.
// Returns the repo path and a cleanup function for any worktrees created outside t.TempDir.
func setupTestRepo(t *testing.T) (repoPath string, worktreeDir string) {
	t.Helper()

	// Create a parent temp directory that will hold both the repo and any worktrees
	parentDir := t.TempDir()

	// Resolve symlinks (macOS /var -> /private/var) so paths match git output
	parentDir, err := filepath.EvalSymlinks(parentDir)
	require.NoError(t, err, "failed to resolve symlinks")

	// Create repo inside the parent dir
	repoPath = filepath.Join(parentDir, "test-repo")
	require.NoError(t, os.MkdirAll(repoPath, 0755), "failed to create repo dir")

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = repoPath
	require.NoError(t, cmd.Run(), "failed to init git repo")

	// Configure git user for commits
	_ = exec.Command("git", "-C", repoPath, "config", "user.email", "test@example.com").Run()
	_ = exec.Command("git", "-C", repoPath, "config", "user.name", "Test User").Run()

	// Create initial commit
	testFile := filepath.Join(repoPath, "README.md")
	require.NoError(t, os.WriteFile(testFile, []byte("# Test Repo\n"), 0644), "failed to create test file")

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = repoPath
	require.NoError(t, cmd.Run(), "failed to git add")

	cmd = exec.Command("git", "commit", "-m", "Initial commit")
	cmd.Dir = repoPath
	require.NoError(t, cmd.Run(), "failed to git commit")

	// Rename branch to main
	cmd = exec.Command("git", "branch", "-M", "main")
	cmd.Dir = repoPath
	_ = cmd.Run() // Ignore error if already main

	return repoPath, parentDir
}

// withWorkingDir runs a function with the working directory changed to dir
func withWorkingDir(t *testing.T, dir string, fn func()) {
	t.Helper()
	oldWd, err := os.Getwd()
	require.NoError(t, err, "failed to get working dir")
	require.NoErrorf(t, os.Chdir(dir), "failed to chdir to %s", dir)
	defer func() { _ = os.Chdir(oldWd) }()
	fn()
}

// normalizePath converts path separators to forward slashes for consistent comparison
// This is needed because git always outputs forward slashes, even on Windows
func normalizePath(p string) string {
	return filepath.ToSlash(p)
}

func TestGetGitInfo(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		info, err := GetGitInfo()
		require.NoError(t, err, "GetGitInfo failed")

		assert.Equal(t, normalizePath(repoPath), normalizePath(info.RepoRoot))
		assert.False(t, info.IsWorktree, "expected IsWorktree=false for main repo")

		expectedName := filepath.Base(repoPath)
		assert.Equal(t, expectedName, info.RepoName)
	})
}

func TestGetDefaultBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		branch, err := GetDefaultBranch()
		require.NoError(t, err, "GetDefaultBranch failed")

		assert.Truef(t, branch == "main" || branch == "master", "expected branch to be main or master, got %s", branch)
	})
}

func TestBranchExists(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		assert.True(t, BranchExists("main"), "expected main branch to exist")
		assert.False(t, BranchExists("nonexistent-branch"), "expected nonexistent-branch to not exist")
	})
}

func TestListWorktrees(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Initially should have just one worktree (main)
		worktrees, err := ListWorktrees()
		require.NoError(t, err, "ListWorktrees failed")

		assert.Len(t, worktrees, 1)

		if len(worktrees) > 0 {
			assert.True(t, worktrees[0].IsMain, "expected first worktree to be marked as main")
			assert.Equal(t, "main", worktrees[0].Branch)
		}
	})
}

func TestWorktreeAddAndRemove(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create a worktree inside the parent temp dir (will be auto-cleaned)
		worktreePath := filepath.Join(parentDir, "test-repo-feature")

		params := &AddParams{
			Branch:   "feature-test",
			Path:     worktreePath,
			Detached: true, // Don't try to start a session
		}

		err := runAdd(params)
		require.NoError(t, err, "runAdd failed")

		// Verify worktree was created
		_, statErr := os.Stat(worktreePath)
		assert.False(t, os.IsNotExist(statErr), "worktree directory was not created")

		// Verify it appears in list
		worktrees, err := ListWorktrees()
		require.NoError(t, err, "ListWorktrees failed")

		assert.Len(t, worktrees, 2)

		// Find the new worktree
		var found bool
		for _, wt := range worktrees {
			if wt.Branch == "feature-test" {
				found = true
				assert.Equal(t, normalizePath(worktreePath), normalizePath(wt.Path))
			}
		}
		assert.True(t, found, "feature-test worktree not found in list")

		// Test FindWorktreeByBranch
		wt, err := FindWorktreeByBranch("feature-test")
		if assert.NoError(t, err, "FindWorktreeByBranch failed") {
			assert.Equal(t, normalizePath(worktreePath), normalizePath(wt.Path), "FindWorktreeByBranch returned wrong path")
		}

		// Test FindWorktreeByPath - use normalized path for lookup on Windows
		wt, err = FindWorktreeByPath(normalizePath(worktreePath))
		if assert.NoError(t, err, "FindWorktreeByPath failed") {
			assert.Equal(t, "feature-test", wt.Branch, "FindWorktreeByPath returned wrong branch")
		}

		// Remove the worktree
		removeParams := &RemoveParams{
			Target: "feature-test",
			Force:  true,
		}

		err = runRemove(removeParams)
		require.NoError(t, err, "runRemove failed")

		// Verify worktree was removed
		worktrees, err = ListWorktrees()
		require.NoError(t, err, "ListWorktrees failed")

		assert.Len(t, worktrees, 1)
	})
}

func TestAddWorktreePathAlreadyExists(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create a directory where the worktree would go
		existingPath := filepath.Join(parentDir, "test-repo-exists")
		require.NoError(t, os.MkdirAll(existingPath, 0755), "failed to create existing dir")

		params := &AddParams{
			Branch:   "exists-test",
			Path:     existingPath,
			Detached: true,
		}

		err := runAdd(params)
		require.Error(t, err, "expected error when path already exists")
		assert.Contains(t, err.Error(), "already exists", "expected 'already exists' error")
	})
}

func TestAddWorktreeWithExistingBranch(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create a branch first
		cmd := exec.Command("git", "branch", "existing-branch")
		cmd.Dir = repoPath
		require.NoError(t, cmd.Run(), "failed to create branch")

		// Create worktree for existing branch
		worktreePath := filepath.Join(parentDir, "test-repo-existing")

		params := &AddParams{
			Branch:   "existing-branch",
			Path:     worktreePath,
			Detached: true,
		}

		err := runAdd(params)
		require.NoError(t, err, "runAdd failed for existing branch")

		// Verify
		wt, err := FindWorktreeByBranch("existing-branch")
		if assert.NoError(t, err, "FindWorktreeByBranch failed") {
			assert.Equal(t, normalizePath(worktreePath), normalizePath(wt.Path), "wrong path for existing branch worktree")
		}
	})
}

func TestRemoveMainWorktree(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		params := &RemoveParams{
			Target: "main",
			Force:  true,
		}

		err := runRemove(params)
		require.Error(t, err, "expected error when removing main worktree")
		assert.Contains(t, err.Error(), "cannot remove the main worktree", "expected 'cannot remove main' error")
	})
}

func TestGetBranchCompletions(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create some branches
		for _, branch := range []string{"feature-a", "feature-b", "bugfix-1"} {
			cmd := exec.Command("git", "branch", branch)
			cmd.Dir = repoPath
			_ = cmd.Run()
		}

		completions := GetBranchCompletions()
		assert.GreaterOrEqual(t, len(completions), 4, "expected at least 4 completions") // main + 3 new branches

		// Check that our branches are in there
		branchSet := make(map[string]bool)
		for _, c := range completions {
			branchSet[c] = true
		}

		for _, expected := range []string{"main", "feature-a", "feature-b", "bugfix-1"} {
			assert.Truef(t, branchSet[expected], "expected %s in completions", expected)
		}
	})
}

func TestAddWorktreeWithSlashInBranchName(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Branch with slash should create path with "--" instead
		params := &AddParams{
			Branch:   "feat/my-feature",
			Detached: true,
		}

		err := runAdd(params)
		require.NoError(t, err, "runAdd failed")

		// The path should be test-repo-feat--my-feature, not test-repo-feat/my-feature
		expectedPath := filepath.Join(parentDir, "test-repo-feat--my-feature")
		_, statErr := os.Stat(expectedPath)
		assert.Falsef(t, os.IsNotExist(statErr), "expected worktree at %s but it doesn't exist", expectedPath)

		// Verify the branch name is preserved in git
		wt, err := FindWorktreeByBranch("feat/my-feature")
		if assert.NoError(t, err, "FindWorktreeByBranch failed") {
			assert.Equal(t, normalizePath(expectedPath), normalizePath(wt.Path))
		}
	})
}
