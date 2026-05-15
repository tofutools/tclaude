package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// repo.go holds repo-path-aware twins of the CWD-implicit helpers in
// worktree.go. The CLI variants run `git` in the process's working
// directory, which is fine for an interactive `tclaude worktree …`
// invocation. The agentd daemon, by contrast, needs to inspect and
// mutate worktrees for an *arbitrary* repo (the one a spawn/clone is
// targeting), so every git call here is explicitly anchored with a
// directory rather than relying on os.Getwd().

// gitIn runs a git command anchored at dir. dir may be any path inside
// the repo — git walks up to the repo root itself. On failure the
// returned error carries git's stderr so callers can surface a useful
// message rather than a bare exit code.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg != "" {
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
			}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// asExitError is errors.As specialised to *exec.ExitError, kept local
// so gitIn doesn't need to pull errors into its import set just for
// one type assertion.
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// RepoRootForPath returns the absolute repo root of the git repository
// containing path. Errors if path doesn't exist or isn't inside a git
// repo — both are conditions the caller wants to report distinctly
// from "no worktrees".
func RepoRootForPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no path given")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not an accessible directory", path)
	}
	root, err := gitIn(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", path)
	}
	return root, nil
}

// parseWorktreePorcelain turns `git worktree list --porcelain` output
// into WorktreeInfo records. Shared by the CWD-implicit ListWorktrees
// and the repo-anchored ListWorktreesIn.
func parseWorktreePorcelain(output string) []WorktreeInfo {
	var worktrees []WorktreeInfo
	var current WorktreeInfo

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = WorktreeInfo{}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "bare":
			current.IsBare = true
		}
	}
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}
	if len(worktrees) > 0 {
		worktrees[0].IsMain = true
	}
	return worktrees
}

// ListWorktreesIn returns all worktrees of the repo containing dir.
func ListWorktreesIn(dir string) ([]WorktreeInfo, error) {
	out, err := gitIn(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}
	return parseWorktreePorcelain(out), nil
}

// BranchesIn returns the deduplicated short branch names (local +
// remote, origin/ prefix stripped) of the repo containing dir. Used to
// populate the "base branch" picker when creating a worktree.
func BranchesIn(dir string) []string {
	out, err := gitIn(dir, "branch", "-a", "--format=%(refname:short)")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "->") {
			continue
		}
		branch := strings.TrimPrefix(line, "origin/")
		if !seen[branch] {
			seen[branch] = true
			branches = append(branches, branch)
		}
	}
	return branches
}

// DefaultBranchIn returns the repo's default branch (origin/HEAD if
// known, else the first of main/master that exists).
func DefaultBranchIn(dir string) (string, error) {
	if ref, err := gitIn(dir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		parts := strings.Split(ref, "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1], nil
		}
	}
	for _, branch := range []string{"main", "master"} {
		if branchExistsIn(dir, branch) {
			return branch, nil
		}
	}
	return "", fmt.Errorf("could not determine default branch (tried main, master)")
}

// branchExistsIn reports whether branch resolves in the repo at dir.
func branchExistsIn(dir, branch string) bool {
	_, err := gitIn(dir, "rev-parse", "--verify", "--quiet", branch)
	return err == nil
}

// AddWorktreeIn creates a git worktree for branch in the repo
// containing repoPath, and returns the absolute path of the new
// worktree. If branch already exists it is checked out; otherwise a
// new branch is created from fromBranch (defaults to the repo's
// default branch). path, when non-empty, overrides the default
// `../<repo>-<branch>` location.
//
// This is the non-printing, repo-anchored core of RunAdd — RunAdd
// stays as the chatty CLI front door; the agentd worktree endpoint
// calls this.
func AddWorktreeIn(repoPath, branch, fromBranch, path string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("branch name is required")
	}
	repoRoot, err := RepoRootForPath(repoPath)
	if err != nil {
		return "", err
	}

	branchExists := branchExistsIn(repoRoot, branch)

	baseBranch := strings.TrimSpace(fromBranch)
	if !branchExists {
		if baseBranch == "" {
			baseBranch, err = DefaultBranchIn(repoRoot)
			if err != nil {
				return "", fmt.Errorf("could not determine base branch: %w (specify one explicitly)", err)
			}
		}
		if !branchExistsIn(repoRoot, baseBranch) {
			return "", fmt.Errorf("base branch %q does not exist", baseBranch)
		}
	}

	worktreePath := strings.TrimSpace(path)
	if worktreePath == "" {
		// Default: sibling of the repo root, ../<repo>-<branch>, with
		// slashes in the branch flattened to "--" so a feature branch
		// doesn't create nested directories.
		safeBranch := strings.ReplaceAll(branch, "/", "--")
		safeBranch = strings.ReplaceAll(safeBranch, "\\", "--")
		worktreePath = filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+"-"+safeBranch)
	}
	if !filepath.IsAbs(worktreePath) {
		worktreePath = filepath.Join(repoRoot, worktreePath)
	}
	if _, err := os.Stat(worktreePath); err == nil {
		return "", fmt.Errorf("worktree path already exists: %s", worktreePath)
	}

	var args []string
	if branchExists {
		args = []string{"worktree", "add", worktreePath, branch}
	} else {
		args = []string{"worktree", "add", "-b", branch, worktreePath, baseBranch}
	}
	if _, err := gitIn(repoRoot, args...); err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}
	return worktreePath, nil
}
