package agentd

// memberOpResult is the per-member outcome of a bulk lifecycle op
// (stop / resume). The CLI prints these as a summary table so the
// human can see which members succeeded, which were no-ops, and
// which failed.
type memberOpResult struct {
	// AgentID is the member's stable actor key — the canonical ID the CLI
	// leads with in the result table; ConvID is the live generation behind it.
	AgentID  string   `json:"agent_id,omitempty"`
	ConvID   string   `json:"conv_id"`
	Title    string   `json:"title,omitempty"`
	Action   string   `json:"action"`           // "soft_stopped", "killed", "killed_no_soft_exit", "resumed", "skipped:already_online", "skipped:no_conv_id", "skipped:external_steward", "error"
	Detail   string   `json:"detail,omitempty"` // human-readable note (e.g. error message)
	TmuxSes  string   `json:"tmux_session,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// Changes is a resume's per-field echo of what its sandbox differs in
	// from the one stored at the last launch (relaunchChanges).
	Changes []string `json:"changes,omitempty"`
	// Worktree is the optional worktree+branch cleanup outcome attached by
	// a bulk retire that requested it (delete_worktree). nil on every other
	// bulk op (stop/resume) and on a retire that did not ask for cleanup,
	// so the field is omitted from those responses entirely.
	Worktree *retireWorktreePlan `json:"worktree,omitempty"`
}

type groupOpResp struct {
	Group   string           `json:"group"`
	Action  string           `json:"action"`
	Members []memberOpResult `json:"members"`
	// RhythmsReenabled is the number of group-target cron jobs a resume
	// re-enabled — exactly the rhythms a prior emptying retire auto-disabled
	// (JOH-345). Omitted when zero / for a stop.
	RhythmsReenabled int `json:"rhythms_reenabled,omitempty"`
}
