package agentd

import "github.com/tofutools/tclaude/pkg/claude/agent"

// agentLocationView is the directory/branch block embedded in every
// agent-listing wire shape — /v1/peers, group members, the dashboard
// snapshot rows. It lets a UI show how far an agent has moved from
// where Claude Code launched it: into a worktree of a sub-repo, or a
// hop between repos mid-session.
//
// `branch` is the agent's *current* branch (resolved from the dir it
// last edited a file in) and stays the primary single value every
// existing reader already consumes. The startup_* fields describe the
// launch dir; current_dir is the live worktree root. A dashboard
// shows one line when startup and current agree, and stacks an
// init/now pair when they diverge.
type agentLocationView struct {
	Branch        string `json:"branch,omitempty"`         // current branch
	StartupDir    string `json:"startup_dir,omitempty"`    // Claude Code launch dir
	StartupBranch string `json:"startup_branch,omitempty"` // git branch of the launch dir
	CurrentDir    string `json:"current_dir,omitempty"`    // git worktree root being worked in now
}

// locationView resolves convID into the embedded location block via
// agent.ResolveLocation — the one resolver every surface routes
// through so directory/branch changes propagate uniformly.
func locationView(convID string) agentLocationView {
	return locationViewFrom(agent.ResolveLocation(convID))
}

// locationViewFrom maps a resolved agent.Location onto the embedded wire block.
// Split out so the dashboard snapshot's per-request batch loader can build the
// view from agent.ResolveLocationFromParts (TCL-368) without re-fetching.
func locationViewFrom(loc agent.Location) agentLocationView {
	return agentLocationView{
		Branch:        loc.CurrentBranch,
		StartupDir:    loc.StartupDir,
		StartupBranch: loc.StartupBranch,
		CurrentDir:    loc.CurrentDir,
	}
}
