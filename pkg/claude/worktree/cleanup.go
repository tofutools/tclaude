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
	root = strings.TrimSpace(root)
	if root == "" {
		return false, nil
	}
	if _, err := os.Stat(root); err != nil {
		// Directory already gone — nothing to remove.
		return false, nil
	}
	// Anchor the removal at the MAIN worktree: `git worktree remove`
	// run from inside the worktree being removed is unreliable.
	wts, err := ListWorktreesIn(root)
	if err != nil {
		return false, fmt.Errorf("list worktrees: %w", err)
	}
	mainPath := ""
	for _, wt := range wts {
		if wt.IsMain {
			mainPath = wt.Path
			break
		}
	}
	if mainPath == "" || sameDir(mainPath, root) {
		return false, fmt.Errorf("refusing to remove the main worktree %s", root)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, root)
	if _, err := gitIn(mainPath, args...); err != nil {
		if isNotAWorktreeErr(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
