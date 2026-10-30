package harness

// AgentViewDisableEnvVar turns off Claude Code's agent view, the session list
// its left arrow opens, along with the background daemon behind it.
//
// tclaude defaults it off, because the agent view takes a conversation out from
// under a managed pane. Leaving for it forks the conversation to a new id inside a
// `claude daemon` PTY, outside tmux, and the pane's process stays on as the list
// UI. Everything tclaude knows about the session then describes the wrong thing:
// the fork's hooks claim TCLAUDE_SESSION_ID but carry no TMUX or TMUX_PANE, so
// the broker refuses them as unplaceable; messages injected into the pane land
// in the list's "new session" prompt; and quitting the list ends the pane and
// marks the session exited while the fork keeps running where nothing can reach
// it. An operator who accepts that can set the `agent-view` context feature to
// on; see session.ApplyAgentViewEnv.
//
// Observed against Claude Code 2.1.312. Re-verify when upgrading: with the
// variable set to 1, the footer shows no "← for agents" hint and left arrow only
// moves the cursor; set empty, the view is available.
const AgentViewDisableEnvVar = "CLAUDE_CODE_DISABLE_AGENT_VIEW"

// SupportsAgentView reports whether the harness has an agent view tclaude must
// turn off. Gated on the harness NAME for the same reason as SupportsAutoMemory:
// the switch is a plain environment variable with nothing per-harness to probe.
func (h *Harness) SupportsAgentView() bool {
	return h != nil && h.Name == DefaultName
}
