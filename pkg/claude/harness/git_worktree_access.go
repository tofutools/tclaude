package harness

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitCommonDir resolves the repository-wide Git metadata directory for cwd.
// For a linked worktree this is the original repository's .git directory,
// rather than the per-worktree metadata directory named by the worktree's
// .git file.
func GitCommonDir(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", nil
	}
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", nil
		}
		return "", fmt.Errorf("resolve git common dir for %q: %w", cwd, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("git common dir for %q resolved to non-absolute path %q", cwd, dir)
	}
	return filepath.Clean(dir), nil
}

// GitWorktreeWriteDirs returns the extra repository paths a sandboxed agent
// needs in order to create and use tclaude's default sibling worktrees:
//
//   - the repository container where ../<repo>-<branch> is created;
//   - the original/main worktree; and
//   - the shared Git common directory.
//
// The container grant is deliberately omitted when it would cover home (or an
// ancestor of home). Granting that path would make ~/.tclaude, ~/.codex, and
// ~/.claude writable and undo the sandbox's protected-state posture. The main
// worktree and common-dir grants remain narrow descendants and are retained.
func GitWorktreeWriteDirs(gitCommonDir, home string) []string {
	gitCommonDir = filepath.Clean(strings.TrimSpace(gitCommonDir))
	if gitCommonDir == "." || !filepath.IsAbs(gitCommonDir) {
		return nil
	}

	var candidates []string
	if filepath.Base(gitCommonDir) == ".git" {
		mainWorktree := filepath.Dir(gitCommonDir)
		candidates = append(candidates, filepath.Dir(mainWorktree), mainWorktree)
	}
	candidates = append(candidates, gitCommonDir)

	home = filepath.Clean(strings.TrimSpace(home))
	seen := map[string]bool{}
	out := make([]string, 0, len(candidates))
	for _, dir := range candidates {
		dir = filepath.Clean(dir)
		if dir == "." || !filepath.IsAbs(dir) || seen[dir] {
			continue
		}
		if home != "." && filepath.IsAbs(home) && pathContains(dir, home) {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

// IsDefaultSiblingWorktree reports whether cwd is a linked worktree at the
// exact location AddWorktreeIn chooses by default: a sibling named
// <main-repo>-<branch>. gitCommonDir must have been resolved from cwd, which
// proves the candidate belongs to that repository rather than merely sharing
// its filename prefix.
func IsDefaultSiblingWorktree(cwd, gitCommonDir string) bool {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	gitCommonDir = filepath.Clean(strings.TrimSpace(gitCommonDir))
	if !filepath.IsAbs(cwd) || !filepath.IsAbs(gitCommonDir) || filepath.Base(gitCommonDir) != ".git" {
		return false
	}
	mainWorktree := filepath.Dir(gitCommonDir)
	if filepath.Dir(cwd) != filepath.Dir(mainWorktree) || cwd == mainWorktree {
		return false
	}
	return strings.HasPrefix(filepath.Base(cwd), filepath.Base(mainWorktree)+"-")
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
