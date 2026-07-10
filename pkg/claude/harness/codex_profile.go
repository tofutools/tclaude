package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// CodexAgentProfile is the name of the tclaude-managed Codex permission
// profile that a daemon-spawned (sandboxed, unattended) Codex agent runs
// under. It mirrors the built-in `:workspace` posture (cwd-subtree writable,
// $HOME read-only), denies all access to tclaude's private ~/.tclaude state,
// and allowlists agentd's separate state-free Unix socket so the agent can run
// `tclaude agent …`. At spawn time it also grants the launch repository's Git
// common dir so the agent can commit from a linked worktree without making the
// rest of $HOME writable.
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
// absolute agent-facing socket and private-state paths. When gitCommonDir is
// non-empty, the profile grants the narrow repository path set
// GitWorktreeWriteDirs derives from it: the sibling-worktree container,
// original/main worktree, and shared Git metadata. That lets the agent create
// a default ../<repo>-<branch> worktree and commit from it without making the
// rest of $HOME writable. Paths are
// embedded as TOML basic-string keys, so a path carrying a double-quote,
// backslash, or control char is rejected rather than allowed to corrupt the
// file (absolute Unix paths never contain those — defence in depth against a
// malformed $HOME or cwd).
//
// The profile:
//   - extends the built-in `:workspace` profile (cwd-subtree writable, $HOME
//     read-only), then denies all access to ~/.tclaude. The canonical agentd
//     socket lives outside that private-state tree;
//   - optionally allows writes to the current repository's main worktree, Git
//     common dir (objects/refs/logs), and safe sibling-worktree container;
//   - enables the network sandbox and allowlists exactly the agentd socket.
//     `network.enabled = true` is REQUIRED for the Unix-socket allowlist to
//     take effect (verified: with it unset the socket connect is denied). It
//     currently also permits general outbound traffic; narrowing that to
//     socket-only is a tracked follow-up;
//   - sets `default_permissions` so `codex -p <name>` activates the profile —
//     the TUI/exec have no `-P` flag, so selection is via default_permissions.
func codexAgentProfileContent(socketPath, privateStateDir, gitCommonDir string) (string, error) {
	if err := validateCodexProfilePath("agentd socket path", socketPath); err != nil {
		return "", err
	}
	if err := validateCodexProfilePath("tclaude private state dir", privateStateDir); err != nil {
		return "", err
	}
	if gitCommonDir != "" {
		if err := validateCodexProfilePath("git common dir", gitCommonDir); err != nil {
			return "", err
		}
	}
	writeDirs := GitWorktreeWriteDirs(gitCommonDir, filepath.Dir(privateStateDir))
	for _, dir := range writeDirs {
		if err := validateCodexProfilePath("git worktree write dir", dir); err != nil {
			return "", err
		}
	}
	p := CodexAgentProfile
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by tclaude (TCL-283) — do not edit; regenerated by `tclaude setup`\n")
	fmt.Fprintf(&b, "# and at spawn time. Selected per-spawn via `codex -p %s` for\n", p)
	fmt.Fprintf(&b, "# tclaude-spawned Codex agents so they can reach the agentd Unix socket\n")
	fmt.Fprintf(&b, "# (`tclaude agent …`) while staying sandboxed.\n\n")
	fmt.Fprintf(&b, "default_permissions = %q\n\n", p)
	fmt.Fprintf(&b, "[permissions.%s]\n", p)
	fmt.Fprintf(&b, "extends = \":workspace\"\n\n")
	fmt.Fprintf(&b, "[permissions.%s.filesystem]\n", p)
	fmt.Fprintf(&b, "%q = \"none\"\n", privateStateDir)
	fmt.Fprintf(&b, "%q = \"read\"\n", socketPath)
	if len(writeDirs) > 0 {
		for _, dir := range writeDirs {
			fmt.Fprintf(&b, "%q = \"write\"\n", dir)
		}
		fmt.Fprintln(&b)
	} else {
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "[permissions.%s.network]\n", p)
	fmt.Fprintf(&b, "enabled = true\n\n")
	fmt.Fprintf(&b, "[permissions.%s.network.unix_sockets]\n", p)
	fmt.Fprintf(&b, "%q = \"allow\"\n", socketPath)
	return b.String(), nil
}

func validateCodexProfilePath(label, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q is not absolute", label, path)
	}
	if strings.ContainsAny(path, "\"\\") || strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 }) {
		return fmt.Errorf("%s %q contains characters unsafe for a TOML key", label, path)
	}
	return nil
}

// EnsureCodexAgentProfile writes the managed tclaude-agent profile file (for
// the canonical state-free agentd socket path) and returns its path. It is
// idempotent
// and self-healing: the file is fully tclaude-owned, so it is (re)written to
// the canonical content whenever the on-disk bytes differ — safe to call from
// `tclaude setup` AND on every spawn. The codex config dir is created if
// missing. Written 0600 (it only references a socket path, but matches the
// private posture of the rest of ~/.codex).
func EnsureCodexAgentProfile() (string, error) {
	sock, privateStateDir, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	return ensureCodexAgentProfile(sock, privateStateDir, "")
}

// EnsureCodexAgentProfileForCwd is the spawn-time variant of
// EnsureCodexAgentProfile. It preserves the base tclaude-agent posture and, if
// cwd is inside a Git repository, adds a write grant for that repository's Git
// common dir. That is the narrow extra permission `git commit` needs from a
// linked worktree whose .git pointer targets metadata outside the workspace
// root. If cwd is not in a Git repo (or git cannot answer), the base profile is
// written.
func EnsureCodexAgentProfileForCwd(cwd string) (string, error) {
	sock, privateStateDir, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	gitCommonDir, err := codexGitCommonDir(cwd)
	if err != nil {
		return "", err
	}
	return ensureCodexAgentProfile(sock, privateStateDir, gitCommonDir)
}

// EnsureCodexAgentProfileForGitCommonDir is the daemon-spawn variant used
// after agentd has already resolved, proved, and pinned a Git common dir. It
// intentionally does not recompute from cwd in the forked session launcher.
func EnsureCodexAgentProfileForGitCommonDir(gitCommonDir string) (string, error) {
	sock, privateStateDir, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	gitCommonDir = strings.TrimSpace(gitCommonDir)
	if gitCommonDir != "" {
		gitCommonDir = filepath.Clean(gitCommonDir)
	}
	return ensureCodexAgentProfile(sock, privateStateDir, gitCommonDir)
}

// CodexGitCommonDir resolves the Git common dir for cwd. Daemon spawn paths use
// this before dir write-proof verification so linked-worktree metadata grants
// are proven and pinned instead of being recomputed after launch.
func CodexGitCommonDir(cwd string) (string, error) {
	return GitCommonDir(cwd)
}

func codexAgentSandboxPaths() (socketPath, privateStateDir string, err error) {
	socketPath = agentipc.CanonicalSocketPath()
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return socketPath, filepath.Join(home, ".tclaude"), nil
}

// ensureCodexAgentProfile is the socket-path-injected core of
// EnsureCodexAgentProfile, split out so tests can drive it without depending
// on the caller's $HOME layout.
func ensureCodexAgentProfile(socketPath, privateStateDir, gitCommonDir string) (string, error) {
	content, err := codexAgentProfileContent(socketPath, privateStateDir, gitCommonDir)
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
	// Atomic write (temp + rename, same dir) so a concurrent spawn or an
	// interrupted setup can never make Codex read a half-written TOML — it sees
	// either the old file or the complete new one. Reuses the harness package's
	// atomicWriteFile, same as EnsureCodexDirTrusted.
	if err := atomicWriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

func codexGitCommonDir(cwd string) (string, error) {
	return GitCommonDir(cwd)
}

// CodexAgentProfileStatus reports the managed profile's on-disk state WITHOUT
// writing — for `tclaude setup --check`, which must stay read-only. It returns
// the file path, whether it exists (present), and whether its bytes match the
// canonical content EnsureCodexAgentProfile would write (current). A present
// but non-current file is a stale/corrupt profile the next spawn self-heals.
func CodexAgentProfileStatus() (path string, present, current bool, err error) {
	sock, privateStateDir, err := codexAgentSandboxPaths()
	if err != nil {
		return "", false, false, err
	}
	baseWant, err := codexAgentProfileContent(sock, privateStateDir, "")
	if err != nil {
		return "", false, false, err
	}
	var acceptable []string
	acceptable = append(acceptable, baseWant)
	if cwd, gerr := os.Getwd(); gerr == nil {
		if gitCommonDir, gerr := codexGitCommonDir(cwd); gerr == nil && gitCommonDir != "" {
			if want, gerr := codexAgentProfileContent(sock, privateStateDir, gitCommonDir); gerr == nil {
				acceptable = append(acceptable, want)
			}
		}
	}
	path, err = CodexAgentProfilePath()
	if err != nil {
		return "", false, false, err
	}
	cur, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return path, false, false, nil
		}
		return path, false, false, rerr
	}
	for _, want := range acceptable {
		if string(cur) == want {
			return path, true, true, nil
		}
	}
	return path, true, false, nil
}
