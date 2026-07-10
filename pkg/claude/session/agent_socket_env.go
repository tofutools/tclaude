package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAgentSocketEnv pins launch modes whose sandbox explicitly allowlists
// agentd to the canonical state-free socket. It refuses the upgrade edge where
// an old daemon is still listening only on a legacy home path (~/.tclaude-agentd.sock
// or ~/.tclaude/agentd.sock): the canonical socket now lives in a per-user
// runtime dir, and both the managed Codex profile and forced-on Claude sandbox
// pin the agent to it, so launching against an old daemon would create an agent
// that cannot coordinate.
func ApplyAgentSocketEnv(harnessName, sandboxMode, permissionProfile string, env map[string]string) error {
	requiresCanonical := (harnessName == harness.CodexName && permissionProfile == harness.CodexAgentProfile) ||
		(harnessName == harness.DefaultName && sandboxMode == harness.ClaudeSandboxOn)
	if !requiresCanonical {
		return nil
	}
	canonical := agentipc.CanonicalSocketPath()
	if explicit := agentipc.ExplicitSocketPath(); explicit != "" && explicit != canonical {
		return fmt.Errorf("managed sandbox profiles require the canonical agentd socket %s; "+
			"custom socket %s is unsupported for sandboxed agents", canonical, explicit)
	}
	if !agentipc.SocketReachable(canonical) && agentipc.AnyLegacySocketReachable() {
		return fmt.Errorf("agentd is still listening only on a legacy home socket; "+
			"restart agentd after upgrading tclaude before launching a sandboxed agent "+
			"(canonical socket: %s)", canonical)
	}
	if canonical != "" {
		env[agentipc.SocketEnv] = canonical
	}
	return nil
}
