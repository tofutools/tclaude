package agentd

import "github.com/tofutools/tclaude/pkg/claude/common/agentipc"

// SocketPath is the canonical, state-free location for agentd's Unix socket
// (a per-user runtime directory, never under $HOME).
func SocketPath() string {
	return agentipc.CanonicalSocketPath()
}

// LegacyHomeSocketPath (~/.tclaude-agentd.sock) is the pre-runtime-dir
// canonical, kept as a compatibility listener during the migration window so an
// already-running agent pinned to it still connects.
func LegacyHomeSocketPath() string { return agentipc.LegacyHomeSocketPath() }

// LegacySocketPath (~/.tclaude/agentd.sock) is the oldest location, kept live
// during the migration window for older clients and previously installed Claude
// sandbox settings.
func LegacySocketPath() string { return agentipc.LegacySocketPath() }
