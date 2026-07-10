package agentd

import "github.com/tofutools/tclaude/pkg/claude/common/agentipc"

// SocketPath is the well-known location for the daemon's Unix socket.
// Clients use it directly (curl --unix-socket, tclaude agent CLI fallback,
// etc.) — no port-discovery file needed.
func SocketPath() string {
	return agentipc.OperatorSocketPath()
}

// SandboxedAgentSocketPath is the state-free endpoint exposed to sandboxed
// Codex agents. The daemon serves the same authenticated API on both sockets.
func SandboxedAgentSocketPath() string { return agentipc.SandboxedAgentSocketPath() }
