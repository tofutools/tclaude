package agentd

import "github.com/tofutools/tclaude/pkg/claude/common/agentipc"

// SocketPath is the canonical, agent-reachable location for agentd's Unix
// socket (~/.tclaude/api/agentd.sock).
func SocketPath() string {
	return agentipc.CanonicalSocketPath()
}

// LegacySocketPaths are the pre-split endpoints agentd keeps binding during the
// migration window so older clients and previously installed sandbox settings
// still reach a restarted daemon.
func LegacySocketPaths() []string { return agentipc.LegacySocketPaths() }
