package agent

import (
	"time"

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
//     edit dir and its git worktree root;
//   - agent_workspace (written by the statusbar on every CC render) →
//     the launch dir's live branch + cwd, fresher than conv_index's
//     per-turn cadence.
//
// StartupBranch is immutable — the branch the FIRST turn was stamped
// with — so a UI can show a stable "init". CurrentBranch is the launch
// dir's live branch, unless the agent has moved into a worktree
// distinct from its launch dir, in which case that worktree's own
// branch is the current one.
//
// Freshness across the three writers — the "most-recent wins" rule:
//
//   - For the LAUNCH-DIR case (the agent has not moved, or its
//     PostToolUse edits are in-tree of the launch dir), conv_index and
//     agent_workspace both describe the same world-state. Pick whichever
//     timestamp is newer; agent_workspace usually wins because the
//     statusbar fires on CC's render cadence while conv_index only
//     refreshes on a turn append — which fixes the "branch flipped but
//     dashboard stayed on the previous one for minutes" lag.
//   - For the MOVED case (the agent has edited a file in a worktree
//     distinct from the launch dir), agent_workdir is the only writer
//     that can describe that worktree at all, so it stays in charge —
//     the statusbar can only see CC's launch dir, never the worktree.
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

	// Track the timestamp of the writer that last refreshed the
	// launch-dir branch claim, so agent_workspace can supersede it
	// when its own row is newer.
	var launchBranchTs time.Time
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
		launchBranchTs = row.IndexedAt
	}

	// A fresh agent that hasn't edited anything is "working" right
	// where it launched — default current dir to startup.
	loc.EditDir = loc.StartupDir
	loc.CurrentDir = loc.StartupDir

	moved := false
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
		// Only a genuine worktree divergence — the agent edits in a
		// repo distinct from where Claude Code launched — overrides the
		// launch-dir branch with the edit worktree's own branch.
		if loc.CurrentDir != "" && loc.CurrentDir != loc.StartupDir {
			loc.CurrentBranch = workdirBranch
			moved = true
		}
	}

	// agent_workspace — the statusbar's render-cadence snapshot of CC's
	// workspace. Only relevant for the launch-dir case (the statusbar
	// can't see a worktree the agent has moved into via Bash); for that
	// case, prefer its branch over conv_index when its row is newer.
	if !moved {
		if ws, err := db.GetAgentWorkspace(convID); err == nil && ws.ConvID != "" {
			if ws.Branch != "" && ws.UpdatedAt.After(launchBranchTs) {
				loc.CurrentBranch = ws.Branch
			}
			// Cwd from the statusbar is CC's launch dir — useful when
			// sessions.cwd / conv_index.project_path didn't resolve a
			// startup dir at all (a corrupt early-life conv).
			if ws.Cwd != "" && loc.CurrentDir == "" {
				loc.CurrentDir = ws.Cwd
				if loc.StartupDir == "" {
					loc.StartupDir = ws.Cwd
				}
				if loc.EditDir == "" {
					loc.EditDir = ws.Cwd
				}
			}
		}
	}
	return loc
}
