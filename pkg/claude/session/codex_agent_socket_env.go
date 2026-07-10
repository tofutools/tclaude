package session

import (
	"fmt"
	"net"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyCodexAgentSocketEnv points managed-profile Codex sessions at agentd's
// canonical state-free socket. It refuses the upgrade edge where an old daemon
// is still listening only inside ~/.tclaude: the new profile deliberately
// denies that tree, so launching would create an agent that cannot coordinate.
// Other sessions retain the normal client socket resolution.
func ApplyCodexAgentSocketEnv(permissionProfile string, env map[string]string) error {
	if permissionProfile != harness.CodexAgentProfile {
		return nil
	}
	canonical := agentipc.CanonicalSocketPath()
	legacy := agentipc.LegacySocketPath()
	if !unixSocketReachable(canonical) && unixSocketReachable(legacy) {
		return fmt.Errorf("agentd is still listening only on the legacy socket %s; "+
			"restart agentd after upgrading tclaude before launching a managed Codex agent", legacy)
	}
	if canonical != "" {
		env[agentipc.SocketEnv] = canonical
	}
	return nil
}

func unixSocketReachable(path string) bool {
	if path == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
