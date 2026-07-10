package agentipc

import (
	"os"
	"path/filepath"
	"strings"
)

// SocketEnv selects the agentd Unix socket used by tclaude agent clients.
// tclaude injects it only into sandboxed Codex sessions, whose filesystem
// profile cannot reach the operator socket below ~/.tclaude.
const SocketEnv = "TCLAUDE_AGENTD_SOCKET"

// OperatorSocketPath is the long-standing agentd socket used by the operator
// and by harnesses that can carve the socket out of a denied ~/.tclaude tree.
func OperatorSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "agentd.sock")
}

// SandboxedAgentSocketPath is the state-free socket endpoint exposed to Codex
// agents. It deliberately lives outside ~/.tclaude so Codex can hide that
// entire private state tree while keeping daemon communication available.
func SandboxedAgentSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude-agentd.sock")
}

// ClientSocketPath resolves the endpoint used by tclaude agent commands. Only
// an absolute override is accepted; an invalid environment value falls back to
// the operator socket rather than turning a relative path into an ambient-CWD
// socket lookup.
func ClientSocketPath() string {
	if path := strings.TrimSpace(os.Getenv(SocketEnv)); filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return OperatorSocketPath()
}
