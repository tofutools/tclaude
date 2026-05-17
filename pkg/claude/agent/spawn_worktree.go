package agent

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// spawn_worktree.go is the CLI-side worktree resolution behind
// `tclaude agent spawn --worktree`. The dashboard's spawn modal resolves
// a worktree directory through its picker (the agentd /api/worktrees
// endpoint) before POSTing the spawn; the CLI has no picker, so it does
// the same git operation in-process here and then sends the identical
// cwd / worktree_path / worktree_branch wire shape. The actual git
// commands live in the worktree package — the single source of truth
// for worktree creation, shared with `tclaude worktree` and agentd.

// resolveSpawnWorktree turns a `--worktree <branch>` request into a
// concrete worktree directory, mirroring what the dashboard's worktree
// picker does: reuse an existing worktree already checked out on that
// branch, otherwise create a fresh one (a new branch cut from `base`,
// or a checkout of an existing branch).
//
// repoDir is any path inside the target git repo; it is resolved up to
// the repo root. createdNew reports whether a worktree was created (vs
// an existing one reused) so the caller can tear it back down if the
// subsequent spawn fails.
func resolveSpawnWorktree(repoDir, branch, base string) (path string, createdNew bool, err error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", false, fmt.Errorf("worktree branch name is required")
	}
	root, err := worktree.RepoRootForPath(repoDir)
	if err != nil {
		return "", false, fmt.Errorf("--worktree needs a git repo: %w", err)
	}
	// Reuse an existing worktree already checked out on this branch —
	// the CLI equivalent of picking it from the dashboard's list.
	wts, err := worktree.ListWorktreesIn(root)
	if err != nil {
		return "", false, fmt.Errorf("list worktrees in %s: %w", root, err)
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			return wt.Path, false, nil
		}
	}
	// None yet — create one. AddWorktreeIn checks out an existing branch
	// or cuts a new one off `base` (the repo's default branch when base
	// is empty), and picks the default `../<repo>-<branch>` location.
	created, err := worktree.AddWorktreeIn(root, branch, strings.TrimSpace(base), "")
	if err != nil {
		return "", false, fmt.Errorf("create worktree: %w", err)
	}
	return created, true, nil
}

// removeSpawnWorktree tears down a worktree resolveSpawnWorktree
// created, used when the spawn it was created for then fails. Only the
// working directory is removed — the branch and its commits stay, so a
// retry reuses the branch. Idempotent: an already-gone worktree is a
// no-op.
func removeSpawnWorktree(path string) (bool, error) {
	return worktree.RemoveLinkedWorktree(path, true)
}
