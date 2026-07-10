package agentd

import "github.com/tofutools/tclaude/pkg/claude/common/agentipc"

// SocketPath is the canonical, state-free location for agentd's Unix socket.
func SocketPath() string {
	return agentipc.CanonicalSocketPath()
}

// LegacySocketPath remains live during the migration window for older clients
// and previously installed Claude sandbox settings.
func LegacySocketPath() string { return agentipc.LegacySocketPath() }
