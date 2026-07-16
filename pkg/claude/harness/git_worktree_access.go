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
	return gitRevParseDir(cwd, "--git-common-dir")
}

// GitDir resolves the checkout-specific Git metadata directory for cwd. For a
// linked worktree this is <common-dir>/worktrees/<name>. Codex protects that
// exact path with a read-only bind even when a writable parent was explicitly
// granted, so sandbox profiles must name it separately for git add/commit.
func GitDir(cwd string) (string, error) {
	return gitRevParseDir(cwd, "--git-dir")
}

func gitRevParseDir(cwd, field string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", nil
	}
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--path-format=absolute", field)
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", nil
		}
		return "", fmt.Errorf("resolve git metadata dir for %q: %w", cwd, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("git metadata dir for %q resolved to non-absolute path %q", cwd, dir)
	}
	return filepath.Clean(dir), nil
}

// GitWorktreeWriteDirs returns the repository paths a sandboxed agent needs in
// order to create and use tclaude's default sibling worktrees. When safe, the
// broad root is the repository container where ../<repo>-<branch> is created;
// the grant also covers the original/main worktree and shared Git metadata.
// When cwd resolves to a checkout, its exact Git admin dir is added separately
// because Codex's protected-.git mount overrides a writable ancestor.
//
// The container grant is deliberately omitted when it would cover home (or an
// ancestor of home). Granting that path would make ~/.tclaude, ~/.codex, and
// ~/.claude writable and undo the sandbox's protected-state posture. The main
// worktree grant remains a narrow descendant and is retained.
func GitWorktreeWriteDirs(cwd, gitCommonDir, home string) []string {
	gitDir, _ := GitDir(cwd)
	return GitWorktreeWriteDirsForIdentity(gitCommonDir, gitDir, home)
}

// GitWorktreeWriteDirsForIdentity derives repository grants from already
// verified metadata paths. Offline resume uses this form so it never follows a
// mutable cwd/.git indirection after validating durable target provenance.
func GitWorktreeWriteDirsForIdentity(gitCommonDir, gitDir, home string) []string {
	gitCommonDir = filepath.Clean(strings.TrimSpace(gitCommonDir))
	if gitCommonDir == "." || !filepath.IsAbs(gitCommonDir) {
		return nil
	}

	home = filepath.Clean(strings.TrimSpace(home))
	if resolvedHome, err := filepath.EvalSymlinks(home); err == nil {
		home = resolvedHome
	}
	var dirs []string
	if filepath.Base(gitCommonDir) != ".git" {
		dirs = []string{gitCommonDir}
	} else {
		mainWorktree := filepath.Dir(gitCommonDir)
		container := filepath.Dir(mainWorktree)
		if home != "." && filepath.IsAbs(home) && pathContains(container, home) {
			// Granting home (or an ancestor) would expose private agent state. The
			// main worktree is the narrowest root that still covers its .git data.
			dirs = []string{mainWorktree}
		} else {
			dirs = []string{container}
		}
	}

	// Codex recursively protects the checkout's exact Git dir even beneath an
	// explicitly writable parent. An exact write rule has higher specificity
	// and restores only the metadata this checkout needs. Failure to resolve it
	// safely retains the existing parent grant; callers already treat a missing
	// Git answer as a non-repository launch.
	gitDir = filepath.Clean(strings.TrimSpace(gitDir))
	if gitDir != "." && filepath.IsAbs(gitDir) {
		for _, dir := range dirs {
			if dir == gitDir {
				return dirs
			}
		}
		dirs = append(dirs, gitDir)
	}
	return dirs
}

// SandboxWorktreeContainer returns the worktree-container directory that
// GitWorktreeWriteDirs grants as a sandbox write root — the parent of the main
// worktree, where tclaude's default ../<repo>-<branch> siblings are created —
// or "" when there is no such container to protect.
//
// It returns "" when gitCommonDir is not an absolute ".git" path, or when the
// home-guard in GitWorktreeWriteDirs collapsed the grant down to the main
// worktree (a real repository, which already has its own ".git" and needs no
// placeholder). The non-empty result is exactly the directory where an OS
// sandbox that denies writes to ".git" inside a writable root would otherwise
// materialize a bogus ".git" DIRECTORY (a set of /dev/null mount stubs) — which
// breaks `go build` VCS stamping. Callers plant an empty ".git" FILE there so
// the sandbox leaves the path alone and Go (which treats only a ".git"
// DIRECTORY as a VCS root) ignores it.
func SandboxWorktreeContainer(gitCommonDir, home string) string {
	clean := filepath.Clean(strings.TrimSpace(gitCommonDir))
	if clean == "." || !filepath.IsAbs(clean) || filepath.Base(clean) != ".git" {
		return ""
	}
	container := filepath.Dir(filepath.Dir(clean))
	// Reuse GitWorktreeWriteDirs so the home-guard lives in one place: it grants
	// [container] only when the container is safe to expose; a home-guarded run
	// grants [mainWorktree] instead, in which case there is no bare container to
	// protect and we return "".
	dirs := GitWorktreeWriteDirsForIdentity(gitCommonDir, "", home)
	if len(dirs) == 1 && dirs[0] == container {
		return container
	}
	return ""
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
