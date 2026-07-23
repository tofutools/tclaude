package agentipc

const (
	// AgentHintEnvVar marks a process as probably belonging to a managed agent
	// session. It is only a UX hint: daemon authorization must continue to use
	// peer credentials and harness ancestry, never this caller-controlled value.
	AgentHintEnvVar = "TCLAUDE_AGENT_HINT"

	// AgentHintHeader carries AgentHintEnvVar across the CLI/daemon boundary so
	// identity failures can return agent-oriented recovery guidance.
	AgentHintHeader = "X-Tclaude-Agent-Hint"
)
