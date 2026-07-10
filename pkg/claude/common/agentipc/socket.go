package agentipc

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

// SocketEnv explicitly selects the agentd Unix socket used by tclaude agent
// clients. Managed Codex sessions pin it to the canonical endpoint.
const SocketEnv = "TCLAUDE_AGENTD_SOCKET"

// CanonicalSocketPath is the agent-reachable endpoint used by every current
// tclaude client and harness (~/.tclaude/api/agentd.sock). It lives under the
// api/ surface — NOT under data/ — so private daemon state (~/.tclaude/data)
// can be denied to sandboxed agents as one complete subtree while the socket
// stays reachable. See pkg/common.TclaudeAPIDir.
func CanonicalSocketPath() string {
	apiDir := common.TclaudeAPIDir()
	if apiDir == "" {
		return ""
	}
	return filepath.Join(apiDir, "agentd.sock")
}

// LegacyHomeSocketPath is the pre-split canonical endpoint
// (~/.tclaude-agentd.sock, outside ~/.tclaude). Retained as a compatibility
// bind+dial listener during the migration window for older clients and
// previously installed sandbox settings. New code must use CanonicalSocketPath.
func LegacyHomeSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude-agentd.sock")
}

// LegacySocketPath is the oldest endpoint (~/.tclaude/agentd.sock). It sits
// directly under the tclaude root but OUTSIDE data/, so it is not denied by the
// data/ sandbox rule and remains a safe migration-window fallback. New code
// must use CanonicalSocketPath.
func LegacySocketPath() string {
	root := common.TclaudeDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "agentd.sock")
}

// LegacySocketPaths returns every retained pre-split endpoint, in the order a
// client should try them after the canonical path: the pre-split home socket
// first, then the oldest under-root socket. Empty entries (unresolvable home)
// are omitted.
func LegacySocketPaths() []string {
	return dedupeNonEmpty(LegacyHomeSocketPath(), LegacySocketPath())
}

// AnyLegacySocketReachable reports whether a daemon is currently accepting
// connections on any retained legacy endpoint. Managed-launch and setup guards
// use it to refuse spawning a sandboxed agent (or installing hardening) while
// the only live daemon is a pre-split one the agent's sandbox would not reach.
func AnyLegacySocketReachable() bool {
	return slices.ContainsFunc(LegacySocketPaths(), SocketReachable)
}

// dedupeNonEmpty returns paths with empties dropped and duplicates (by cleaned
// form) removed, preserving order.
func dedupeNonEmpty(paths ...string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		key := filepath.Clean(p)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

// ClientSocketPath resolves the preferred endpoint used by tclaude agent
// commands. Only an absolute override is accepted; an invalid environment
// value falls back to the canonical socket rather than turning a relative path
// into an ambient-CWD socket lookup.
func ClientSocketPath() string {
	return ClientSocketPaths()[0]
}

// ExplicitSocketPath returns a valid absolute SocketEnv override, or "" when
// the variable is unset/invalid.
func ExplicitSocketPath() string {
	if path := strings.TrimSpace(os.Getenv(SocketEnv)); filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return ""
}

// ClientSocketPaths returns dial candidates in priority order. An explicit
// absolute override is authoritative. Without one, current clients prefer the
// canonical api/ path and fall back to the retained legacy paths so an updated
// CLI still works with a running older daemon or previously installed sandbox
// settings during the migration window.
func ClientSocketPaths() []string {
	if path := ExplicitSocketPath(); path != "" {
		return []string{path}
	}
	return dedupeNonEmpty(append([]string{CanonicalSocketPath()}, LegacySocketPaths()...)...)
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
