package agentd

import (
	"strings"
)

// ---- wire shapes -----------------------------------------------------------

// tuiAgentRow is the subset of /v1/peers the console renders. Kept as its
// own decode shape (rather than reusing peerEntry) so the console reads only
// the fields it actually shows.
type tuiAgentRow struct {
	AgentID    string        `json:"agent_id"`
	ConvID     string        `json:"conv_id"`
	Title      string        `json:"title"`
	Online     bool          `json:"online"`
	Groups     []string      `json:"groups"`
	Branch     string        `json:"branch"`
	StartupDir string        `json:"startup_dir"`
	CurrentDir string        `json:"current_dir"`
	State      tuiAgentState `json:"state"`
}

// tuiAgentState is the activity subset of /v1/peers' state block.
type tuiAgentState struct {
	Harness    string `json:"harness"`
	Status     string `json:"status"`
	ExitReason string `json:"exit_reason"`
}

// dir is the directory column: where the agent is working now, falling back
// to where it was launched.
func (a tuiAgentRow) dir() string {
	if a.CurrentDir != "" {
		return a.CurrentDir
	}
	return a.StartupDir
}

// name is the display label: the conversation title, falling back to the
// stable agent id for an agent that has not been titled yet.
func (a tuiAgentRow) name() string {
	if t := strings.TrimSpace(a.Title); t != "" {
		return t
	}
	if a.AgentID != "" {
		return a.AgentID
	}
	return a.ConvID
}

// status is the one-word activity summary, with offline taking precedence:
// a stale status from the agent's last live turn would otherwise read as if
// the pane were still up.
func (a tuiAgentRow) status() string {
	if !a.Online {
		if a.State.Harness == "shell" && strings.HasPrefix(a.State.ExitReason, "command_") {
			return strings.TrimPrefix(a.State.ExitReason, "command_")
		}
		return "offline"
	}
	if s := strings.TrimSpace(a.State.Status); s != "" {
		return s
	}
	return "online"
}

// tuiGroupRow is the subset of /v1/groups the console needs: the name is
// what the spawn form picks from and posts to, and the default directory is
// what it starts that group's spawn in.
type tuiGroupRow struct {
	Name string `json:"name"`
	// DefaultCwd is the group's configured spawn directory, "" when it has
	// none — and always "" for a console the daemon does not treat as the
	// human, which is not served the path at all.
	DefaultCwd string `json:"default_cwd,omitempty"`
}

// tuiProfileRow is the subset of /v1/spawn-profiles the spawn form picks
// from. Disabled comes along because a disabled profile is refused at spawn,
// so offering one would only produce a rejection the operator cannot act on
// from here.
//
// AgentName and SyncWorktree are the two profile fields this form has to apply
// ITSELF. Every other one the daemon resolves down its own tier stack, so
// naming the profile is enough — but a worktree is cut before the spawn request
// goes out, from a branch typed into this form, so "name the agent's worktree
// after the agent" has to happen here or not at all.
type tuiProfileRow struct {
	Name     string `json:"name"`
	Disabled *bool  `json:"disabled,omitempty"`
	// AgentName is the profile's display name for the agents it spawns. The
	// daemon applies it to a request that omits `name`; the form prefills it so
	// the operator can see (and extend) the name their worktree branch follows.
	AgentName string `json:"agent_name,omitempty"`
	// SyncWorktree is the profile's "give each agent its own worktree, named
	// after it" toggle — tri-state, nil when the profile says nothing.
	SyncWorktree *bool `json:"sync_worktree,omitempty"`
	// InitialMessage is the profile's task-brief default. The form does not
	// prefill it — a profile's brief may be many lines and the Brief field is a
	// one-line input — so this is read only to SAY that an empty box will be
	// filled by it, rather than leaving the operator to find out afterwards.
	InitialMessage string `json:"initial_message,omitempty"`
}

func (p tuiProfileRow) disabled() bool { return p.Disabled != nil && *p.Disabled }

// syncsWorktree reports whether this profile asks its agents to be spawned into
// a worktree of their own. Unset is not "no": it leaves the picker alone.
func (p tuiProfileRow) syncsWorktree() (bool, bool) {
	if p.SyncWorktree == nil {
		return false, false
	}
	return *p.SyncWorktree, true
}

// tuiRetireResult is the subset of the retire response the console reports:
// what the demotion actually revoked, and what became of the agent's session.
type tuiRetireResult struct {
	Outcome  retireConvOutcome `json:"outcome"`
	Shutdown memberOpResult    `json:"shutdown"`
}

// tuiStopResult is the subset of the stop response the console reports.
type tuiStopResult struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// tuiResumeResult is the subset of the resume response the console reports:
// what the daemon did, and — for everything that is not a plain "resumed" —
// why.
type tuiResumeResult struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	// Warnings are the notes a resume that DID land still wants read: an
	// agent that came back with reduced sandbox access, say. They are the one
	// thing a bare "Started X." would otherwise swallow, and this console has
	// no browser to go and find them in.
	Warnings []string `json:"warnings,omitempty"`
}
