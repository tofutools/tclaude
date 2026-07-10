package agentipc

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SocketEnv explicitly selects the agentd Unix socket used by tclaude agent
// clients. Managed Codex sessions pin it to the canonical state-free endpoint.
const SocketEnv = "TCLAUDE_AGENTD_SOCKET"

// socketBasename is the socket filename inside the per-user runtime directory.
const socketBasename = "agentd.sock"

// CanonicalSocketPath is the state-free endpoint every current tclaude client
// and harness prefers: /tmp/tclaude-<uid>/agentd.sock. It lives in a per-user
// subdirectory of /tmp — a stable, always-available runtime location outside
// both ~/.tclaude and $HOME, so private daemon state can be denied as one
// complete subtree while the socket stays reachable, and so the socket sits in
// a runtime location instead of cluttering the home directory.
//
// It depends only on the uid — the LITERAL /tmp, not $TMPDIR/os.TempDir — so
// the daemon and every client/launcher compute an IDENTICAL path with no shared
// environment and no filesystem probing. That determinism is load-bearing: a
// socket the daemon binds is exactly the one a client dials, and a pin or
// sandbox allowlist a launcher computes matches what the daemon actually bound,
// even across a cron/systemd-user daemon and an interactive launcher.
//
// $XDG_RUNTIME_DIR is deliberately NOT used: it is per-process (so a cron daemon
// and a login launcher would disagree), and on some hosts (e.g. WSL without a
// systemd user session) it points at a /run/user/<uid> that exists but is
// read-only, which cannot hold a socket at all. /tmp is writable everywhere
// (Linux, macOS, WSL, cron, bare SSH) and its literal path is short enough to
// stay well under the Unix sun_path limit.
//
// /tmp is world-writable + sticky, so the daemon creates /tmp/tclaude-<uid>
// 0700 and refuses to bind if a pre-existing dir is not owned by the current
// user, defeating a squatted path (see agentd.ensureOwnedRuntimeDir).
func CanonicalSocketPath() string {
	return filepath.Join(tmpRuntimeSocketDir(), socketBasename)
}

// tmpRuntimeSocketDir is the per-uid socket directory under the literal /tmp
// (not $TMPDIR / os.TempDir) so the path is identical for every tclaude process
// regardless of a per-process TMPDIR (a sandboxed child may see a different one).
func tmpRuntimeSocketDir() string {
	return filepath.Join("/tmp", "tclaude-"+strconv.Itoa(os.Getuid()))
}

// LegacyHomeSocketPath is the pre-runtime-dir canonical endpoint,
// ~/.tclaude-agentd.sock. It is retained as a compatibility bind/dial target
// during the migration window so an already-running agent pinned to it, or a
// not-yet-restarted older daemon, still connects. New code must use
// CanonicalSocketPath.
func LegacyHomeSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude-agentd.sock")
}

// LegacySocketPath is the oldest location, inside ~/.tclaude. It is retained as
// a compatibility listener for older clients and previously installed Claude
// sandbox settings. New code must use CanonicalSocketPath.
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
// canonical runtime-dir path, then fall back to the two legacy home paths so a
// client still connects to a not-yet-restarted older daemon (or an already
// running agent) during the migration window.
func ClientSocketPaths() []string {
	if path := ExplicitSocketPath(); path != "" {
		return []string{path}
	}
	return dedupeNonEmpty([]string{
		CanonicalSocketPath(),
		LegacyHomeSocketPath(),
		LegacySocketPath(),
	})
}

// dedupeNonEmpty returns paths with empty entries dropped and later duplicates
// (compared cleaned) removed, preserving first-seen order.
func dedupeNonEmpty(paths []string) []string {
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

// AnyLegacySocketReachable reports whether a daemon is answering on either
// legacy home path but NOT the canonical runtime path — the "old daemon still
// running after a binary upgrade" state the sandbox/hardening preflights refuse
// to launch a pinned agent into.
func AnyLegacySocketReachable() bool {
	return slices.ContainsFunc(
		[]string{LegacyHomeSocketPath(), LegacySocketPath()},
		SocketReachable,
	)
}
