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
// monorepo" launch dir, or any session that hops between repos — or
// whenever the launch repo's branch changes mid-session (a plain
// `git checkout`). StartupBranch is the branch the FIRST turn was
// stamped with and never changes; CurrentBranch tracks the branch the
// agent is on now.
type Location struct {
	StartupDir    string // Claude Code's launch dir (sessions.cwd)
	StartupBranch string // launch-time branch; immutable, "" outside a git repo
	EditDir       string // dir of the agent's most-recent file edit
	CurrentDir    string // git worktree root of EditDir — "where it's working now"
	CurrentBranch string // git branch the agent is on now
	Tracked       bool   // true once the PostToolUse hook has recorded an edit
}

// Moved reports whether the agent's current worktree differs from its
// launch dir — the signal a UI uses to decide between showing one
// location line or stacking a startup/current pair.
func (l Location) Moved() bool {
	return l.CurrentDir != "" && l.CurrentDir != l.StartupDir
}

// ResolveLocation assembles an agent's Location from the stores that
// hold the pieces:
//
//   - sessions.cwd + the conv_index row → the startup dir, the startup
//     branch (conv_index.git_branch_startup, first-wins) and the
//     launch dir's current branch (conv_index.git_branch, last-wins);
//   - agent_workdir (written by the PostToolUse hook) → the current
//     edit dir and its git worktree root.
//
// StartupBranch is immutable — the branch the FIRST turn was stamped
// with — so a UI can show a stable "init". CurrentBranch is the launch
// dir's last-wins branch, unless the agent has moved into a worktree
// distinct from its launch dir, in which case that worktree's own
// branch is the current one.
//
// An agent that hasn't edited anything yet has no agent_workdir row;
// its current location simply mirrors startup (Tracked == false). A
// row written by a pre-v28 hook (or one whose edit-time git resolution
// failed) carries no worktree_root — ResolveLocation then resolves the
// edit dir's git repo root on demand and heals the row, so "where it's
// working now" is always the repo root rather than a deep sub-path.
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
		// StartupBranch is the FIRST turn's gitBranch — the launch
		// branch, immutable. conv_index.git_branch is last-wins, so it
		// is the launch dir's *current* branch, not the startup one. A
		// conv indexed before schema v32 has no startup branch captured
		// yet — fall back to git_branch so the field is never blank for
		// a git session (it self-heals on the next .jsonl rescan).
		loc.StartupBranch = row.GitBranchStartup
		if loc.StartupBranch == "" {
			loc.StartupBranch = row.GitBranch
		}
		loc.CurrentBranch = row.GitBranch
	}

	// A fresh agent that hasn't edited anything is "working" right
	// where it launched — default current dir to startup.
	loc.EditDir = loc.StartupDir
	loc.CurrentDir = loc.StartupDir

	if w, err := db.GetAgentWorkdir(convID); err == nil && w.Dir != "" {
		loc.Tracked = true
		loc.EditDir = w.Dir
		// CurrentDir mirrors the edit dir until a git root is known.
		loc.CurrentDir = w.Dir
		workdirBranch := w.Branch
		switch {
		case w.WorktreeRoot != "":
			// A v28+ hook already resolved the git root at edit time.
			loc.CurrentDir = w.WorktreeRoot
		default:
			// worktree_root is unset: a row from a pre-v28 hook, or one
			// whose edit-time git resolution failed. Resolve the repo
			// root on demand so the current dir is the repo root, not a
			// deep sub-path — then heal the row so subsequent reads
			// stay pure DB lookups (the v28 no-git-per-refresh goal).
			if root, branch := session.GitLocationOf(w.Dir); root != "" {
				loc.CurrentDir = root
				workdirBranch = branch
				_ = db.HealAgentWorkdirGit(convID, root, branch)
			}
		}
		// CurrentBranch already holds conv_index.git_branch — the
		// launch dir's last-wins branch, the freshest signal for an
		// agent working in its launch repo (Claude Code re-stamps it
		// every turn). Only a genuine worktree divergence — the agent
		// edits in a repo distinct from where Claude Code launched —
		// overrides it with the edit worktree's own branch.
		if loc.CurrentDir != "" && loc.CurrentDir != loc.StartupDir {
			loc.CurrentBranch = workdirBranch
		}
	}
	return loc
}
