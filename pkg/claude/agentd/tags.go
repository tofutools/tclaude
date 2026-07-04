package agentd

// tags.go carries the per-agent tag set — a small set of short label
// strings keyed on the stable agent_id — from the agent_tags table onto
// the dashboard's dashboardMember / dashboardAgent wire shapes. Two kinds
// of tag share the store: free-form operator tags (set from the dashboard
// or `tclaude agent tags`) and the auto-stamped `tf:<template-name>`
// marker recording which task-force / template deployment spawned the
// agent (JOH-380, stamped in spawnWaveAgents).
//
// The datum is agent-authored / operator-authored and dashboard-only — a
// tag never rides a tmux send-keys, so the validation in
// db.NormalizeAgentTag is UI/sanity hygiene (printable, bounded, no
// control chars), not an injection guard. Rendering still esc()s every
// tag in the browser.

// tagsView is the per-row tag block embedded in the dashboard wire
// shapes. omitempty: an agent with no tags contributes nothing and the
// Description cell renders no chips. The slice is the DB's already-sorted
// (alphabetical) set, so the chip order is deterministic across ticks.
type tagsView struct {
	Tags []string `json:"tags,omitempty"`
}
