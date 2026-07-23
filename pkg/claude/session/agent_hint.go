package session

import "github.com/tofutools/tclaude/pkg/claude/common/agentipc"

// applyManagedAgentHint marks agentd-managed launches for best-effort UX
// behavior in descendants, including harness-native subagents. The value is
// not an authorization credential; agentd still resolves identity from the
// Unix-socket peer and process ancestry.
func applyManagedAgentHint(managed bool, env map[string]string) {
	if managed {
		env[agentipc.AgentHintEnvVar] = "1"
	}
}
