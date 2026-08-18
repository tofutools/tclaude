package agentipc

const (
	// AgentHintEnvVar marks a process as probably belonging to a managed agent
	// session. It is only a UX hint: daemon authorization must continue to use
	// peer credentials and harness ancestry, never this caller-controlled value.
	AgentHintEnvVar = "TCLAUDE_AGENT_HINT"

	// AgentHintHeader carries AgentHintEnvVar across the CLI/daemon boundary so
	// identity failures can return agent-oriented recovery guidance.
	AgentHintHeader = "X-Tclaude-Agent-Hint"

	// SessionIDEnvVar is the stable session-row key exported into managed panes.
	// It is caller-controlled and therefore only selects a row for server-side
	// proof; it is never authorization by itself.
	SessionIDEnvVar = "TCLAUDE_SESSION_ID"

	// SessionClaimHeader carries SessionIDEnvVar on agent CLI requests. Agentd
	// accepts it only when Unix peer credentials, the live tmux pane, and the
	// daemon-owned launch generation independently prove the claimed row.
	SessionClaimHeader = "X-Tclaude-Session-ID"
)
