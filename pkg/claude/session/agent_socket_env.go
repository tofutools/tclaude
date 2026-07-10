package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAgentSocketEnv pins launch modes whose sandbox explicitly allowlists
// agentd to the canonical state-free socket. It refuses the upgrade edge where
// an old daemon is still listening only inside ~/.tclaude: both the managed
// Codex profile and forced-on Claude sandbox deliberately deny that tree, so
// launching would create an agent that cannot coordinate.
func ApplyAgentSocketEnv(harnessName, sandboxMode, permissionProfile string, env map[string]string) error {
	requiresCanonical := (harnessName == harness.CodexName && permissionProfile == harness.CodexAgentProfile) ||
		(harnessName == harness.DefaultName && sandboxMode == harness.ClaudeSandboxOn)
	if !requiresCanonical {
		return nil
	}
	canonical := agentipc.CanonicalSocketPath()
	legacy := agentipc.LegacySocketPath()
	if !agentipc.SocketReachable(canonical) && agentipc.SocketReachable(legacy) {
		return fmt.Errorf("agentd is still listening only on the legacy socket %s; "+
			"restart agentd after upgrading tclaude before launching a sandboxed agent", legacy)
	}
	if canonical != "" {
		env[agentipc.SocketEnv] = canonical
	}
	return nil
}
