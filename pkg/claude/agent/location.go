package agent

import (
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Location is an agent's full directory + git-branch picture: where
// Claude Code was launched (the Startup* pair) versus where the agent
// is actually editing right now (the Current* pair).
//
// The two diverge whenever an agent works somewhere other than its
// launch dir — most notably a worktree of a sub-repo inside a "virtual
// monorepo" launch dir, or any session that hops between repos. The
// startup branch is Claude Code's own per-turn gitBranch stamp (the
// launch dir's branch, empty when that dir isn't a git repo); the
// current branch is resolved from the dir the agent last edited a file
// in, recorded by the PostToolUse hook into agent_workdir.
type Location struct {
	StartupDir    string // Claude Code's launch dir (sessions.cwd)
	StartupBranch string // git branch of StartupDir; "" outside a git repo
	EditDir       string // dir of the agent's most-recent file edit
	CurrentDir    string // git worktree root of EditDir — "where it's working now"
	CurrentBranch string // git branch checked out at CurrentDir
	Tracked       bool   // true once the PostToolUse hook has recorded an edit
}

// Moved reports whether the agent's current worktree differs from its
// launch dir — the signal a UI uses to decide between showing one
// location line or stacking a startup/current pair.
func (l Location) Moved() bool {
	return l.CurrentDir != "" && l.CurrentDir != l.StartupDir
}

// ResolveLocation assembles an agent's Location from the two stores
// that hold the pieces:
//
//   - sessions.cwd + the conv_index row → the startup dir & branch;
//   - agent_workdir (written by the PostToolUse hook) → the current
//     edit dir, its git worktree root, and its branch.
//
// An agent that hasn't edited anything yet has no agent_workdir row;
// its current location simply mirrors startup (Tracked == false). A
// row written by a pre-v28 hook (or one whose edit-time git resolution
// failed) carries no worktree_root/branch — ResolveLocation then
// resolves the edit dir's git repo root + branch on demand and heals
// the row, so "where it's working now" is always the repo root rather
// than a deep sub-path. An edit dir outside any git repo keeps the
// edit dir, with no branch.
//
// This is the branch sibling of FreshTitle: every surface that renders
// where an agent is working should route through it so they all pick
// up directory/branch changes uniformly.
func ResolveLocation(convID string) Location {
	var loc Location

	if sess, err := db.FindSessionByConvID(convID); err == nil && sess != nil {
		loc.StartupDir = sess.Cwd
	}
	row := FreshConvRowResolved(convID)
	if row != nil {
		if loc.StartupDir == "" {
			loc.StartupDir = row.ProjectPath
		}
		loc.StartupBranch = row.GitBranch
	}

	// A fresh agent that hasn't edited anything is "working" right
	// where it launched — default current to startup.
	loc.EditDir = loc.StartupDir
	loc.CurrentDir = loc.StartupDir
	loc.CurrentBranch = loc.StartupBranch

	if w, err := db.GetAgentWorkdir(convID); err == nil && w.Dir != "" {
		loc.Tracked = true
		loc.EditDir = w.Dir
		// Default to the recorded values; CurrentDir mirrors the edit
		// dir until a git root is known.
		loc.CurrentDir = w.Dir
		loc.CurrentBranch = w.Branch
		switch {
		case w.WorktreeRoot != "":
			// A v28+ hook already resolved the git root + branch at
			// edit time — trust it. An empty branch here is a real
			// detached HEAD, not missing data.
			loc.CurrentDir = w.WorktreeRoot
		default:
			// worktree_root is unset: a row from a pre-v28 hook, or one
			// whose edit-time git resolution failed. Resolve the repo
			// root on demand so the current dir is the repo root, not a
			// deep sub-path — then heal the row so subsequent reads
			// stay pure DB lookups (the v28 no-git-per-refresh goal).
			// An edit dir outside any git repo resolves to nothing and
			// keeps the edit dir / empty branch, as before.
			if root, branch := session.GitLocationOf(w.Dir); root != "" {
				loc.CurrentDir = root
				loc.CurrentBranch = branch
				_ = db.HealAgentWorkdirGit(convID, root, branch)
			}
		}
	}
	return loc
}
