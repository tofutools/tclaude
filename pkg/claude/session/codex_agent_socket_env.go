package session

import (
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyCodexAgentSocketEnv points managed-profile Codex sessions at agentd's
// state-free socket. Other sessions retain the operator socket default.
func ApplyCodexAgentSocketEnv(permissionProfile string, env map[string]string) {
	if permissionProfile != harness.CodexAgentProfile {
		return
	}
	if sock := agentipc.SandboxedAgentSocketPath(); sock != "" {
		env[agentipc.SocketEnv] = sock
	}
}
