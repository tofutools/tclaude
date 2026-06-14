package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CodexAgentProfile is the name of the tclaude-managed Codex permission
// profile that a daemon-spawned (sandboxed, unattended) Codex agent runs
// under. It mirrors the built-in `:workspace` posture (cwd-subtree writable,
// $HOME read-only) and additionally allowlists the agentd Unix socket so the
// agent can run `tclaude agent …` (JOH-207).
//
// It is realised as a layered config-profile file the Spawner selects with
// `codex -p <name>` — NOT a `--sandbox` flag — because the two sandbox models
// do not compose: whenever a `sandbox_mode` / `--sandbox` is in play Codex
// uses the older sandbox settings and silently ignores permission profiles
// (verified firsthand against codex-cli 0.139.0; JOH-207). And only the
// permission-profile
// model can allowlist a single Unix socket — the legacy
// `[sandbox_workspace_write]` table has all-or-nothing `network_access` only.
const CodexAgentProfile = "tclaude-agent"

// codexProfileNameRe restricts a permission-profile name to a simple
// identifier. The name becomes a `codex -p <name>` launch arg, the
// <name>.config.toml filename, AND a TOML table key, so confining it to
// letters/digits/'_'/'-' blocks path traversal and any shell/TOML
// metacharacter at the boundary where untrusted input could enter (the
// human-facing --permission-profile flag).
var codexProfileNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateCodexProfileName trims and validates a Codex permission-profile
// name. "" passes through unchanged (the caller omits the flag); any other
// value must match codexProfileNameRe or it is rejected.
func ValidateCodexProfileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if !codexProfileNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid codex permission profile name %q (allowed: letters, digits, '_', '-')", name)
	}
	return name, nil
}

// defaultAgentdSocketPath mirrors agentd.SocketPath()'s well-known default
// (~/.tclaude/agentd.sock). It is duplicated here rather than imported to keep
// the harness package free of an agentd dependency (agentd imports harness).
// A daemon run on a non-default `--socket` is out of scope: the managed
// profile allowlists the standard path, which is what daemon-spawned agents
// connect to.
func defaultAgentdSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tclaude", "agentd.sock"), nil
}

// codexConfigDir returns Codex's config home: $CODEX_HOME when set, else
// ~/.codex. `codex -p <name>` resolves <name>.config.toml against exactly this
// dir, and a daemon-spawned codex inherits the same environment as the
// `tclaude session new` that forked it, so the managed profile must be written
// here for the `-p` selection to find it.
func codexConfigDir() (string, error) {
	if h := strings.TrimSpace(os.Getenv("CODEX_HOME")); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// CodexAgentProfilePath is the managed profile file that `codex -p
// tclaude-agent` resolves: <codex-home>/tclaude-agent.config.toml.
func CodexAgentProfilePath() (string, error) {
	dir, err := codexConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CodexAgentProfile+".config.toml"), nil
}

// codexAgentProfileContent renders the managed profile's TOML for the given
// absolute agentd socket path. The path is embedded as a TOML basic-string
// key, so a path carrying a double-quote, backslash, or control char is
// rejected rather than allowed to corrupt the file (an absolute socket path
// never contains those — defence in depth against a malformed $HOME).
//
// The profile:
//   - extends the built-in `:workspace` profile (cwd-subtree writable, $HOME
//     read-only) — the same containment tclaude's previous `--sandbox
//     workspace-write` gave, so the guardrail-integrity property is preserved;
//   - enables the network sandbox and allowlists exactly the agentd socket.
//     `network.enabled = true` is REQUIRED for the Unix-socket allowlist to
//     take effect (verified: with it unset the socket connect is denied). It
//     currently also permits general outbound traffic; narrowing that to
//     socket-only is a tracked follow-up;
//   - sets `default_permissions` so `codex -p <name>` activates the profile —
//     the TUI/exec have no `-P` flag, so selection is via default_permissions.
func codexAgentProfileContent(socketPath string) (string, error) {
	if !filepath.IsAbs(socketPath) {
		return "", fmt.Errorf("agentd socket path %q is not absolute", socketPath)
	}
	if strings.ContainsAny(socketPath, "\"\\") || strings.ContainsFunc(socketPath, func(r rune) bool { return r < 0x20 }) {
		return "", fmt.Errorf("agentd socket path %q contains characters unsafe for a TOML key", socketPath)
	}
	p := CodexAgentProfile
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by tclaude (JOH-207) — do not edit; regenerated by `tclaude setup`\n")
	fmt.Fprintf(&b, "# and at spawn time. Selected per-spawn via `codex -p %s` for\n", p)
	fmt.Fprintf(&b, "# tclaude-spawned Codex agents so they can reach the agentd Unix socket\n")
	fmt.Fprintf(&b, "# (`tclaude agent …`) while staying sandboxed.\n\n")
	fmt.Fprintf(&b, "default_permissions = %q\n\n", p)
	fmt.Fprintf(&b, "[permissions.%s]\n", p)
	fmt.Fprintf(&b, "extends = \":workspace\"\n\n")
	fmt.Fprintf(&b, "[permissions.%s.network]\n", p)
	fmt.Fprintf(&b, "enabled = true\n\n")
	fmt.Fprintf(&b, "[permissions.%s.network.unix_sockets]\n", p)
	fmt.Fprintf(&b, "%q = \"allow\"\n", socketPath)
	return b.String(), nil
}

// EnsureCodexAgentProfile writes the managed tclaude-agent profile file (for
// the well-known agentd socket path) and returns its path. It is idempotent
// and self-healing: the file is fully tclaude-owned, so it is (re)written to
// the canonical content whenever the on-disk bytes differ — safe to call from
// `tclaude setup` AND on every spawn. The codex config dir is created if
// missing. Written 0600 (it only references a socket path, but matches the
// private posture of the rest of ~/.codex).
func EnsureCodexAgentProfile() (string, error) {
	sock, err := defaultAgentdSocketPath()
	if err != nil {
		return "", err
	}
	return ensureCodexAgentProfile(sock)
}

// ensureCodexAgentProfile is the socket-path-injected core of
// EnsureCodexAgentProfile, split out so tests can drive it without depending
// on the caller's $HOME layout.
func ensureCodexAgentProfile(socketPath string) (string, error) {
	content, err := codexAgentProfileContent(socketPath)
	if err != nil {
		return "", err
	}
	path, err := CodexAgentProfilePath()
	if err != nil {
		return "", err
	}
	// Skip the write when the file already holds the canonical content, so a
	// per-spawn ensure() doesn't churn the mtime on the hot path.
	if cur, rerr := os.ReadFile(path); rerr == nil && string(cur) == content {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create codex config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
