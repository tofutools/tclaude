package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupTestRepo creates a temporary git repository for testing.
// Returns the repo path and a cleanup function for any worktrees created outside t.TempDir.
func setupTestRepo(t *testing.T) (repoPath string, worktreeDir string) {
	t.Helper()

	// Create a parent temp directory that will hold both the repo and any worktrees
	parentDir := t.TempDir()

	// Resolve symlinks (macOS /var -> /private/var) so paths match git output
	parentDir, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		t.Fatalf("failed to resolve symlinks: %v", err)
	}

	// Create repo inside the parent dir
	repoPath = filepath.Join(parentDir, "test-repo")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("failed to create repo dir: %v", err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Configure git user for commits
	exec.Command("git", "-C", repoPath, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", repoPath, "config", "user.name", "Test User").Run()

	// Create initial commit
	testFile := filepath.Join(repoPath, "README.md")
	if err := os.WriteFile(testFile, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git add: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Initial commit")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git commit: %v", err)
	}

	// Rename branch to main
	cmd = exec.Command("git", "branch", "-M", "main")
	cmd.Dir = repoPath
	cmd.Run() // Ignore error if already main

	return repoPath, parentDir
}

// withWorkingDir runs a function with the working directory changed to dir
func withWorkingDir(t *testing.T, dir string, fn func()) {
	t.Helper()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working dir: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to %s: %v", dir, err)
	}
	defer os.Chdir(oldWd)
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
		if err != nil {
			t.Fatalf("GetGitInfo failed: %v", err)
		}

		if normalizePath(info.RepoRoot) != normalizePath(repoPath) {
			t.Errorf("expected RepoRoot=%s, got %s", repoPath, info.RepoRoot)
		}

		if info.IsWorktree {
			t.Error("expected IsWorktree=false for main repo")
		}

		expectedName := filepath.Base(repoPath)
		if info.RepoName != expectedName {
			t.Errorf("expected RepoName=%s, got %s", expectedName, info.RepoName)
		}
	})
}

func TestGetDefaultBranch(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		branch, err := GetDefaultBranch()
		if err != nil {
			t.Fatalf("GetDefaultBranch failed: %v", err)
		}

		if branch != "main" && branch != "master" {
			t.Errorf("expected branch to be main or master, got %s", branch)
		}
	})
}

func TestBranchExists(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		if !BranchExists("main") {
			t.Error("expected main branch to exist")
		}

		if BranchExists("nonexistent-branch") {
			t.Error("expected nonexistent-branch to not exist")
		}
	})
}

func TestListWorktrees(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Initially should have just one worktree (main)
		worktrees, err := ListWorktrees()
		if err != nil {
			t.Fatalf("ListWorktrees failed: %v", err)
		}

		if len(worktrees) != 1 {
			t.Errorf("expected 1 worktree, got %d", len(worktrees))
		}

		if len(worktrees) > 0 {
			if !worktrees[0].IsMain {
				t.Error("expected first worktree to be marked as main")
			}
			if worktrees[0].Branch != "main" {
				t.Errorf("expected branch=main, got %s", worktrees[0].Branch)
			}
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
		if err != nil {
			t.Fatalf("runAdd failed: %v", err)
		}

		// Verify worktree was created
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			t.Error("worktree directory was not created")
		}

		// Verify it appears in list
		worktrees, err := ListWorktrees()
		if err != nil {
			t.Fatalf("ListWorktrees failed: %v", err)
		}

		if len(worktrees) != 2 {
			t.Errorf("expected 2 worktrees, got %d", len(worktrees))
		}

		// Find the new worktree
		var found bool
		for _, wt := range worktrees {
			if wt.Branch == "feature-test" {
				found = true
				if normalizePath(wt.Path) != normalizePath(worktreePath) {
					t.Errorf("expected path=%s, got %s", worktreePath, wt.Path)
				}
			}
		}
		if !found {
			t.Error("feature-test worktree not found in list")
		}

		// Test FindWorktreeByBranch
		wt, err := FindWorktreeByBranch("feature-test")
		if err != nil {
			t.Errorf("FindWorktreeByBranch failed: %v", err)
		} else if normalizePath(wt.Path) != normalizePath(worktreePath) {
			t.Errorf("FindWorktreeByBranch returned wrong path")
		}

		// Test FindWorktreeByPath - use normalized path for lookup on Windows
		wt, err = FindWorktreeByPath(normalizePath(worktreePath))
		if err != nil {
			t.Errorf("FindWorktreeByPath failed: %v", err)
		} else if wt.Branch != "feature-test" {
			t.Errorf("FindWorktreeByPath returned wrong branch")
		}

		// Remove the worktree
		removeParams := &RemoveParams{
			Target: "feature-test",
			Force:  true,
		}

		err = runRemove(removeParams)
		if err != nil {
			t.Fatalf("runRemove failed: %v", err)
		}

		// Verify worktree was removed
		worktrees, err = ListWorktrees()
		if err != nil {
			t.Fatalf("ListWorktrees failed: %v", err)
		}

		if len(worktrees) != 1 {
			t.Errorf("expected 1 worktree after removal, got %d", len(worktrees))
		}
	})
}

func TestAddWorktreePathAlreadyExists(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create a directory where the worktree would go
		existingPath := filepath.Join(parentDir, "test-repo-exists")
		if err := os.MkdirAll(existingPath, 0755); err != nil {
			t.Fatalf("failed to create existing dir: %v", err)
		}

		params := &AddParams{
			Branch:   "exists-test",
			Path:     existingPath,
			Detached: true,
		}

		err := runAdd(params)
		if err == nil {
			t.Error("expected error when path already exists")
		}

		if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("expected 'already exists' error, got: %v", err)
		}
	})
}

func TestAddWorktreeWithExistingBranch(t *testing.T) {
	repoPath, parentDir := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create a branch first
		cmd := exec.Command("git", "branch", "existing-branch")
		cmd.Dir = repoPath
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to create branch: %v", err)
		}

		// Create worktree for existing branch
		worktreePath := filepath.Join(parentDir, "test-repo-existing")

		params := &AddParams{
			Branch:   "existing-branch",
			Path:     worktreePath,
			Detached: true,
		}

		err := runAdd(params)
		if err != nil {
			t.Fatalf("runAdd failed for existing branch: %v", err)
		}

		// Verify
		wt, err := FindWorktreeByBranch("existing-branch")
		if err != nil {
			t.Errorf("FindWorktreeByBranch failed: %v", err)
		} else if normalizePath(wt.Path) != normalizePath(worktreePath) {
			t.Errorf("wrong path for existing branch worktree")
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
		if err == nil {
			t.Error("expected error when removing main worktree")
		}

		if !strings.Contains(err.Error(), "cannot remove the main worktree") {
			t.Errorf("expected 'cannot remove main' error, got: %v", err)
		}
	})
}

func TestGetBranchCompletions(t *testing.T) {
	repoPath, _ := setupTestRepo(t)

	withWorkingDir(t, repoPath, func() {
		// Create some branches
		for _, branch := range []string{"feature-a", "feature-b", "bugfix-1"} {
			cmd := exec.Command("git", "branch", branch)
			cmd.Dir = repoPath
			cmd.Run()
		}

		completions := GetBranchCompletions()
		if len(completions) < 4 { // main + 3 new branches
			t.Errorf("expected at least 4 completions, got %d", len(completions))
		}

		// Check that our branches are in there
		branchSet := make(map[string]bool)
		for _, c := range completions {
			branchSet[c] = true
		}

		for _, expected := range []string{"main", "feature-a", "feature-b", "bugfix-1"} {
			if !branchSet[expected] {
				t.Errorf("expected %s in completions", expected)
			}
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
		if err != nil {
			t.Fatalf("runAdd failed: %v", err)
		}

		// The path should be test-repo-feat--my-feature, not test-repo-feat/my-feature
		expectedPath := filepath.Join(parentDir, "test-repo-feat--my-feature")
		if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
			t.Errorf("expected worktree at %s but it doesn't exist", expectedPath)
		}

		// Verify the branch name is preserved in git
		wt, err := FindWorktreeByBranch("feat/my-feature")
		if err != nil {
			t.Errorf("FindWorktreeByBranch failed: %v", err)
		} else if normalizePath(wt.Path) != normalizePath(expectedPath) {
			t.Errorf("expected path=%s, got %s", expectedPath, wt.Path)
		}
	})
}
