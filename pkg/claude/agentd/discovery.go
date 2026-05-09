package agentd

import (
	"os"
	"path/filepath"
)

// SocketPath is the well-known location for the daemon's Unix socket.
// Clients use it directly (curl --unix-socket, tclaude agent CLI fallback,
// etc.) — no port-discovery file needed.
func SocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "agentd.sock")
}
