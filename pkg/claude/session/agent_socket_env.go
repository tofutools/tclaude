package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAgentSocketEnv pins launch modes whose sandbox explicitly allowlists
// agentd to the canonical api/ socket. It refuses the upgrade edge where an old
// daemon is still listening only on a pre-split legacy socket that the current
// sandbox posture does not allowlist, so launching would create an agent that
// cannot coordinate. (The retained legacy sockets sit outside ~/.tclaude/data,
// which is the only denied subtree; this guard is about a daemon that has not
// yet restarted onto the api/ canonical path.)
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
		return fmt.Errorf("agentd is still listening only on a pre-split legacy socket (%v); "+
			"restart agentd after upgrading tclaude before launching a sandboxed agent",
			agentipc.LegacySocketPaths())
	}
	if canonical != "" {
		env[agentipc.SocketEnv] = canonical
	}
	return nil
}
