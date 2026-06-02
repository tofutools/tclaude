package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cleanup.go is the worktree-removal half of agent cleanup. The
// dashboard's "delete agent" / "🧹 cleanup" flows offer to remove the
// git worktree a deleted agent was working in; these are the
// repo-anchored, non-interactive helpers that back that — siblings of
// AddWorktreeIn, kept apart from the chatty CLI runRemove.

// WorktreeStatus classifies a directory for worktree cleanup.
type WorktreeStatus struct {
	// Root is the git working-tree root containing the queried dir,
	// or "" when the dir isn't inside a git repo.
	Root string
	// Branch is the branch checked out at Root ("HEAD" when detached).
	Branch string
	// Kind is "none" (not in a git repo), "main" (the repo's primary
	// checkout — never removed by cleanup), or "linked" (a
	// `git worktree add` checkout, which cleanup may remove).
	Kind string
}

// InspectWorktree resolves the git worktree containing dir and
// classifies it. A dir that doesn't exist or isn't inside a git repo
// comes back Kind=="none". Main vs linked is decided by the hard git
// invariant that a linked worktree's `.git` is a gitlink *file* while
// the main worktree's is a *directory* — no extra git call needed.
func InspectWorktree(dir string) WorktreeStatus {
	if strings.TrimSpace(dir) == "" {
		return WorktreeStatus{Kind: "none"}
	}
	root, err := RepoRootForPath(dir)
	if err != nil {
		return WorktreeStatus{Kind: "none"}
	}
	st := WorktreeStatus{Root: root, Kind: "main"}
	if fi, serr := os.Stat(filepath.Join(root, ".git")); serr == nil && !fi.IsDir() {
		st.Kind = "linked"
	}
	if b, berr := gitIn(root, "rev-parse", "--abbrev-ref", "HEAD"); berr == nil {
		st.Branch = b
	}
	return st
}

// RemoveLinkedWorktree removes the linked git worktree rooted at root.
//
// It is idempotent and never errors on an already-clean state:
//   - root whose directory is already gone → (false, nil); git prunes
//     the stale registration on its own gc.
//   - root that git no longer knows as a worktree → (false, nil).
//
// force passes `--force`, removing the worktree even with uncommitted
// or untracked changes — the branch and its commits are left intact,
// only the working directory goes. The main worktree is refused: a
// non-nil error comes back rather than nuking the user's primary
// checkout.
func RemoveLinkedWorktree(root string, force bool) (bool, error) {
	removed, _, err := removeLinkedWorktree(root, "" /* keep branch */, force)
	return removed, err
}

// RemoveLinkedWorktreeAndBranch removes the linked worktree rooted at
// root AND force-deletes its local branch. It is the retire-time
// cleanup: where RemoveLinkedWorktree keeps the branch, retiring an
// agent wants its whole git footprint gone — working directory and the
// throwaway feature branch alike.
//
// Both steps are anchored at the repo's MAIN worktree (resolved once,
// before removal): a branch checked out in a worktree can't be deleted,
// and `git worktree remove` is unreliable run from inside the worktree
// being removed.
//
// The branch is deleted with `git branch -D` (force) — the team
// squash-merges via PRs, so a safe `-d` would refuse the common case
// and leave the branch behind. main/master are NEVER deleted (a
// defence-in-depth guard: the trunk must survive even if it is somehow
// the branch checked out in a linked worktree). The worktree directory
// is still removed in that case; only the branch is kept.
//
// Returns:
//   - removed: whether the worktree directory was removed (false when it
//     was already gone or git no longer knew it — the same idempotent
//     contract as RemoveLinkedWorktree).
//   - branchDeleted: whether the branch was deleted. False when the
//     worktree wasn't removed, the branch is empty / "HEAD" / protected,
//     or git reports it already gone.
//   - err: a real git failure on either step, or the main-worktree
//     refusal.
func RemoveLinkedWorktreeAndBranch(root, branch string, force bool) (removed, branchDeleted bool, err error) {
	return removeLinkedWorktree(root, branch, force)
}

// removeLinkedWorktree is the shared core behind RemoveLinkedWorktree
// (branch == "") and RemoveLinkedWorktreeAndBranch (branch set). It
// removes the linked worktree at root and, when branch is a non-empty
// non-protected name, force-deletes it afterwards from the same main
// anchor. branchDeleted is always false when branch == "".
func removeLinkedWorktree(root, branch string, force bool) (removed, branchDeleted bool, err error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return false, false, nil
	}
	if _, statErr := os.Stat(root); statErr != nil {
		// Directory already gone — nothing to remove.
		return false, false, nil
	}
	// Anchor the removal at the MAIN worktree: `git worktree remove`
	// run from inside the worktree being removed is unreliable, and the
	// branch deletion below must run from a checkout that isn't the one
	// being deleted.
	wts, listErr := ListWorktreesIn(root)
	if listErr != nil {
		return false, false, fmt.Errorf("list worktrees: %w", listErr)
	}
	mainPath := ""
	for _, wt := range wts {
		if wt.IsMain {
			mainPath = wt.Path
			break
		}
	}
	if mainPath == "" || sameDir(mainPath, root) {
		return false, false, fmt.Errorf("refusing to remove the main worktree %s", root)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, root)
	if _, rmErr := gitIn(mainPath, args...); rmErr != nil {
		if isNotAWorktreeErr(rmErr) {
			return false, false, nil
		}
		return false, false, rmErr
	}
	removed = true

	// Optional branch deletion (retire only). The worktree is gone now,
	// so the branch is no longer checked out and `git branch -D` works
	// from the main anchor.
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" || isProtectedBranch(branch) {
		return removed, false, nil
	}
	if _, brErr := gitIn(mainPath, "branch", "-D", branch); brErr != nil {
		if isNoSuchBranchErr(brErr) {
			return removed, false, nil
		}
		return removed, false, fmt.Errorf("delete branch %s: %w", branch, brErr)
	}
	return removed, true, nil
}

// isProtectedBranch reports whether branch is the repo trunk that
// cleanup must never delete. Compared case-insensitively so a stray
// "Main"/"MASTER" is caught too — erring toward keeping a branch is
// always the safe direction here.
func isProtectedBranch(branch string) bool {
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master":
		return true
	}
	return false
}

// sameDir reports whether two paths point at the same directory,
// tolerant of symlinks (macOS /var → /private/var) and trailing
// separators.
func sameDir(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	if ra, err := filepath.EvalSymlinks(ca); err == nil {
		ca = ra
	}
	if rb, err := filepath.EvalSymlinks(cb); err == nil {
		cb = rb
	}
	return ca == cb
}

// isNotAWorktreeErr recognises the git failures that mean "there is
// nothing here to remove" — treated as a successful no-op so cleanup
// stays idempotent.
func isNotAWorktreeErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not a working tree") ||
		strings.Contains(msg, "not a working tree") ||
		strings.Contains(msg, "no such file or directory")
}

// isNoSuchBranchErr recognises the `git branch -D` failure that means
// the branch was already gone — treated as a successful no-op so branch
// cleanup stays idempotent (e.g. a re-run, or the branch deleted by
// hand). `git branch -D missing` prints "error: branch 'missing' not
// found." / "... couldn't find ...".
func isNoSuchBranchErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "couldn't find") ||
		strings.Contains(msg, "no such")
}
