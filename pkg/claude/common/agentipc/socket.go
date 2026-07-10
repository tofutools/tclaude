package agentipc

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SocketEnv explicitly selects the agentd Unix socket used by tclaude agent
// clients. Managed Codex sessions pin it to the canonical state-free endpoint.
const SocketEnv = "TCLAUDE_AGENTD_SOCKET"

// CanonicalSocketPath is the state-free endpoint used by every current
// tclaude client and harness. It deliberately lives outside ~/.tclaude so that
// private daemon state can be denied as one complete subtree.
func CanonicalSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude-agentd.sock")
}

// LegacySocketPath is retained as a compatibility listener for older clients
// and previously installed Claude sandbox settings. New code must use
// CanonicalSocketPath.
func LegacySocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "agentd.sock")
}

// ClientSocketPath resolves the preferred endpoint used by tclaude agent
// commands. Only an absolute override is accepted; an invalid environment
// value falls back to the canonical socket rather than turning a relative path
// into an ambient-CWD socket lookup.
func ClientSocketPath() string {
	return ClientSocketPaths()[0]
}

// ClientSocketPaths returns dial candidates in priority order. An explicit
// absolute override is authoritative. Without one, current clients prefer the
// canonical path and fall back to the legacy path so an updated CLI still
// works with a running older daemon or previously installed Claude sandbox
// settings during the migration window.
func ClientSocketPaths() []string {
	if path := strings.TrimSpace(os.Getenv(SocketEnv)); filepath.IsAbs(path) {
		return []string{filepath.Clean(path)}
	}
	paths := []string{CanonicalSocketPath()}
	if legacy := LegacySocketPath(); legacy != "" && legacy != paths[0] {
		paths = append(paths, legacy)
	}
	return paths
}

// SocketReachable reports whether a process is accepting connections at path.
// It is used only for short migration preflights, never as a substitute for an
// authenticated daemon request.
func SocketReachable(path string) bool {
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
