package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
// `codex -p <name>` — the installed baseline is tclaude-agent, while each
// launch uses a unique tclaude-agent-<launch-id> copy so proof-scoped
// repository grants cannot race. This is NOT a `--sandbox` flag because the two sandbox models
// do not compose: whenever a `sandbox_mode` / `--sandbox` is in play Codex
// uses the older sandbox settings and silently ignores permission profiles
// (verified firsthand against codex-cli 0.139.0; JOH-207). And only the
// permission-profile
// model can allowlist a single Unix socket — the legacy
// `[sandbox_workspace_write]` table has all-or-nothing `network_access` only.
const CodexAgentProfile = "tclaude-agent"

const codexAgentLaunchProfileMaxAge = 24 * time.Hour

// codexProfileNameRe restricts a permission-profile name to a simple
// identifier. The name becomes a `codex -p <name>` launch arg, the
// <name>.config.toml filename, AND a TOML table key, so confining it to
// letters/digits/'_'/'-' blocks path traversal and any shell/TOML
// metacharacter at the boundary where untrusted input could enter (the
// human-facing --permission-profile flag).
var codexProfileNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var codexAgentLaunchIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
var codexAgentLaunchProfileFileRe = regexp.MustCompile(`^tclaude-agent-[0-9a-f]{16}\.config\.toml$`)

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
	return codexAgentProfilePath(CodexAgentProfile)
}

func codexAgentProfilePath(name string) (string, error) {
	name, err := ValidateCodexProfileName(name)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("codex permission profile name is required")
	}
	dir, err := codexConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".config.toml"), nil
}

// codexAgentProfileContent renders the managed profile's TOML for the given
// absolute agent-facing socket and private-state paths. When gitCommonDir is
// non-empty, the profile grants the minimal repository root
// GitWorktreeWriteDirs derives from it. That root covers the sibling-worktree
// container, original/main worktree, and shared Git metadata. It lets the agent
// create a default ../<repo>-<branch> worktree and commit from it without making the
// rest of $HOME writable. Paths are
// embedded as TOML basic-string keys, so a path carrying a double-quote,
// backslash, or control char is rejected rather than allowed to corrupt the
// file (absolute Unix paths never contain those — defence in depth against a
// malformed $HOME or cwd).
//
// The profile:
//   - extends the built-in `:workspace` profile (cwd-subtree writable, $HOME
//     read-only), then denies all access to ~/.tclaude/data (the private-state
//     subtree). The canonical agentd socket lives under ~/.tclaude/api, outside
//     that denied tree, so it stays reachable;
//   - optionally allows writes to the minimal stable root covering the current
//     repository and safe sibling-worktree container;
//   - always declares the agentd socket for control-plane access and denies
//     tclaude's tmux server socket directory. Denying the socket is the actual
//     host-control boundary: hiding the tmux binary alone would still permit a
//     copied client or direct protocol implementation to control the server;
//   - maps an explicit Internet/offline network posture to Codex's network
//     boundary. Linux offline is currently rejected because both its native
//     restricted-network seccomp and managed proxy prevent the agent from
//     opening agentd's Unix socket. macOS uses the managed proxy's
//     deny-by-default policy so Seatbelt can carry the socket exception;
//   - sets `default_permissions` so `codex -p <name>` activates the profile —
//     the TUI/exec have no `-P` flag, so selection is via default_permissions.
func codexAgentProfileContent(socketPath, privateStateDir, homeDir, gitCommonDir string) (string, error) {
	if gitCommonDir != "" {
		if err := validateCodexProfilePath("git common dir", gitCommonDir); err != nil {
			return "", err
		}
	}
	// No cwd at this content-render site, so no exact-git-dir grant; homeDir is
	// the real $HOME (privateStateDir is ~/.tclaude/data now, so its parent is
	// no longer home — see codexAgentSandboxPaths).
	writeDirs := GitWorktreeWriteDirs("", gitCommonDir, homeDir)
	return codexAgentProfileContentForWriteDirs(socketPath, privateStateDir, writeDirs)
}

func codexAgentProfileContentForWriteDirs(socketPath, privateStateDir string, writeDirs []string) (string, error) {
	return codexAgentProfileContentForNameAndWriteDirs(CodexAgentProfile, socketPath, privateStateDir, writeDirs)
}

func codexAgentProfileContentForNameAndWriteDirs(profileName, socketPath, privateStateDir string, writeDirs []string) (string, error) {
	return codexAgentProfileContentForNameAndGrants(profileName, socketPath, privateStateDir, nil, writeDirs)
}

func codexAgentProfileContentForNameAndGrants(profileName, socketPath, privateStateDir string, readDirs, writeDirs []string) (string, error) {
	return codexAgentProfileContentForNameAndRules(profileName, socketPath, privateStateDir, readDirs, writeDirs, nil)
}

func codexAgentProfileContentForNameAndRules(profileName, socketPath, privateStateDir string, readDirs, writeDirs, denyDirs []string) (string, error) {
	return codexAgentProfileContentForNameAndRulesAndNetwork(profileName, socketPath, privateStateDir, readDirs, writeDirs, denyDirs, sandboxpolicy.NetworkAccessInherit)
}

func codexAgentProfileContentForNameAndRulesAndNetwork(profileName, socketPath, privateStateDir string, readDirs, writeDirs, denyDirs []string, networkAccess sandboxpolicy.NetworkAccess) (string, error) {
	return codexAgentProfileContentForNameAndRulesAndNetworkForOS(profileName, socketPath, privateStateDir, readDirs, writeDirs, denyDirs, networkAccess, runtime.GOOS)
}

// ValidateCodexAgentNetworkAccess checks whether the current Codex platform can
// enforce the requested external-network posture without severing tclaude's
// agentd Unix socket. Codex's Linux restricted-network seccomp currently denies
// connect(2) for every address family, including AF_UNIX.
func ValidateCodexAgentNetworkAccess(networkAccess sandboxpolicy.NetworkAccess) error {
	return validateCodexAgentNetworkAccessForOS(networkAccess, runtime.GOOS)
}

func validateCodexAgentNetworkAccessForOS(networkAccess sandboxpolicy.NetworkAccess, goos string) error {
	networkAccess, err := sandboxpolicy.NormalizeNetworkAccess(networkAccess)
	if err != nil {
		return err
	}
	if networkAccess == sandboxpolicy.NetworkAccessNone && goos == "linux" {
		return fmt.Errorf("offline network access is unavailable on Linux/WSL because Codex's restricted-network seccomp also blocks the agentd Unix socket")
	}
	return nil
}

// CodexSandboxRules is the complete filesystem/posture input the managed Codex
// permission profile renders. It exists because the parameter list grew past
// the point where positional arguments were safe: read, write, deny, and the
// two break-glass classes are all []string, and transposing two of them would
// silently produce a wrong-but-valid sandbox.
type CodexSandboxRules struct {
	ReadDirs  []string
	WriteDirs []string
	DenyDirs  []string
	// ReadBaseline "minimal" drops `extends = ":workspace"` (whose resolved
	// policy makes the filesystem root readable) in favor of an enumerated
	// allowlist. Codex permission profiles make `extends` optional and an
	// extends-less profile resolves to a deny-all baseline, so this is a real
	// allowlist read posture rather than an approximation.
	ReadBaseline sandboxpolicy.ReadBaseline
	// BreakGlassReadDirs/BreakGlassWriteDirs are acknowledged protected-path
	// exceptions. They must suppress the baseline private-state deny they
	// cover: on Codex a deny dominates any narrower grant regardless of
	// declaration order, so leaving the deny in place would silently discard
	// the operator's acknowledged access.
	BreakGlassReadDirs  []string
	BreakGlassWriteDirs []string
	// RequireSplitPolicy pins the Linux backend away from legacy Landlock.
	// Home exclusions set it only after the isolated behavioral probe proved
	// denied-parent/narrower-reopen semantics for this Codex executable.
	RequireSplitPolicy bool
}

// codexMinimalRuntimeGrants are the special paths a minimal (extends-less)
// profile must still grant for tools to run at all. ":minimal" is Codex's
// purpose-built runtime baseline and expands to the system binary/library
// roots (/bin, /etc, /lib, /lib64, /sbin, /usr); without it an extends-less
// profile cannot execute even /usr/bin/true. The temp roots replace the
// writable /tmp and $TMPDIR that ":workspace" supplied for free.
//
// Note what is deliberately NOT restored: ":root" = "read". That single entry
// is the broad read baseline this whole feature exists to remove.
var codexMinimalRuntimeGrants = []struct {
	key    string
	access string
}{
	{":minimal", "read"},
	{":slash_tmp", "write"},
	{":tmpdir", "write"},
}

func codexAgentProfileContentForNameAndRulesAndNetworkForOS(profileName, socketPath, privateStateDir string, readDirs, writeDirs, denyDirs []string, networkAccess sandboxpolicy.NetworkAccess, goos string) (string, error) {
	return codexAgentProfileContentForRules(profileName, socketPath, privateStateDir,
		CodexSandboxRules{ReadDirs: readDirs, WriteDirs: writeDirs, DenyDirs: denyDirs}, networkAccess, goos)
}

func codexAgentProfileContentForRules(profileName, socketPath, privateStateDir string, rules CodexSandboxRules, networkAccess sandboxpolicy.NetworkAccess, goos string) (string, error) {
	readDirs, writeDirs, denyDirs := rules.ReadDirs, rules.WriteDirs, rules.DenyDirs
	profileName, err := ValidateCodexProfileName(profileName)
	if err != nil {
		return "", err
	}
	if profileName == "" {
		return "", fmt.Errorf("codex permission profile name is required")
	}
	if err := validateCodexProfilePath("agentd socket path", socketPath); err != nil {
		return "", err
	}
	if err := validateCodexProfilePath("tclaude private state dir", privateStateDir); err != nil {
		return "", err
	}
	if err := validateCodexAgentNetworkAccessForOS(networkAccess, goos); err != nil {
		return "", err
	}
	tmuxSocketDir, err := codexTmuxSocketDir()
	if err != nil {
		return "", err
	}
	readBaseline, err := sandboxpolicy.NormalizeReadBaseline(rules.ReadBaseline)
	if err != nil {
		return "", err
	}
	breakGlass := append(append([]string{}, rules.BreakGlassReadDirs...), rules.BreakGlassWriteDirs...)
	allDirs := append(append(append(append([]string{}, readDirs...), writeDirs...), denyDirs...), breakGlass...)
	for _, dir := range allDirs {
		if err := validateCodexProfilePath("sandbox profile directory", dir); err != nil {
			return "", err
		}
	}
	grants := make(map[string]string, len(allDirs))
	for _, dir := range readDirs {
		grants[dir] = "read"
	}
	for _, dir := range writeDirs {
		grants[dir] = "write"
	}
	for _, dir := range denyDirs {
		grants[dir] = "none"
	}
	// Break-glass is applied AFTER the ordinary rules and write after read, so
	// an acknowledged protected grant is never downgraded by an ordinary deny
	// that happens to name the same path. Read still does not imply write.
	for _, dir := range rules.BreakGlassReadDirs {
		grants[dir] = "read"
	}
	for _, dir := range rules.BreakGlassWriteDirs {
		grants[dir] = "write"
	}
	// Claude Code session state remains protected unless an acknowledged
	// break-glass rule covers it. ~/.codex is deliberately not denied here:
	// standalone Codex installations place the executable beneath that root,
	// and the Linux sandbox re-executes it for every tool command.
	protectedRoots, err := codexProtectedRootDenies(privateStateDir)
	if err != nil {
		return "", err
	}
	for _, root := range protectedRoots {
		if breakGlassCoversPath(breakGlass, root) {
			continue
		}
		grants[root] = "none"
	}
	if tmuxSocketDir != "" {
		// The tmux socket directory is host-control authority — a strictly more
		// severe class than protected state — and is NOT reachable through
		// break-glass. It stays denied unconditionally, after every other rule.
		grants[tmuxSocketDir] = "none"
	}
	// The private-state baseline deny is emitted separately below. Avoid a
	// duplicate TOML key when an operator profile repeats it explicitly, UNLESS
	// an acknowledged break-glass rule targets that exact path — in which case
	// the grant must survive and the baseline deny must be suppressed.
	suppressPrivateStateDeny := breakGlassCoversPath(breakGlass, privateStateDir)
	if !suppressPrivateStateDeny {
		delete(grants, privateStateDir)
	}
	grantPaths := make([]string, 0, len(grants))
	for dir := range grants {
		grantPaths = append(grantPaths, dir)
	}
	sort.Strings(grantPaths)
	p := profileName
	useOfflineProxy := networkAccess == sandboxpolicy.NetworkAccessNone && goos == "darwin"
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by tclaude (TCL-283) — do not edit; regenerated by `tclaude setup`\n")
	fmt.Fprintf(&b, "# and at spawn time. Selected per-spawn via `codex -p %s` for\n", p)
	fmt.Fprintf(&b, "# tclaude-spawned Codex agents so they can reach the agentd Unix socket\n")
	fmt.Fprintf(&b, "# (`tclaude agent …`) while staying sandboxed.\n\n")
	fmt.Fprintf(&b, "default_permissions = %q\n\n", p)
	featureNetworkProxy := ""
	if networkAccess == sandboxpolicy.NetworkAccessInternet {
		featureNetworkProxy = "false"
	} else if useOfflineProxy {
		featureNetworkProxy = "true"
	}
	if featureNetworkProxy != "" || rules.RequireSplitPolicy {
		// Use the scalar feature form so this launch profile replaces any
		// feature-level proxy configuration from the user's base config. This
		// prevents inherited allowlists from widening an offline launch and
		// prevents an inherited proxy from narrowing explicit Internet access.
		fmt.Fprintf(&b, "[features]\n")
		if featureNetworkProxy != "" {
			fmt.Fprintf(&b, "network_proxy = %s\n", featureNetworkProxy)
		}
		if rules.RequireSplitPolicy {
			fmt.Fprintf(&b, "use_legacy_landlock = false\n")
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "[permissions.%s]\n", p)
	if readBaseline == sandboxpolicy.ReadBaselineMinimal {
		// No `extends` at all: Codex resolves an extends-less profile to a
		// deny-all filesystem baseline, which is precisely the allowlist read
		// posture a minimal profile asks for. The enumerated runtime grants
		// below replace what ":workspace" used to supply, minus its readable
		// filesystem root.
		fmt.Fprintf(&b, "# read_baseline = minimal: no `extends`, so the filesystem baseline is deny-all\n")
		fmt.Fprintf(&b, "# and only the enumerated grants below are readable.\n\n")
	} else {
		fmt.Fprintf(&b, "extends = \":workspace\"\n\n")
	}
	fmt.Fprintf(&b, "[permissions.%s.filesystem]\n", p)
	if readBaseline == sandboxpolicy.ReadBaselineMinimal {
		for _, grant := range codexMinimalRuntimeGrants {
			fmt.Fprintf(&b, "%q = %q\n", grant.key, grant.access)
		}
		// Harness-owned homes intentionally remain agent-readable unless Home
		// is itself excluded. Codex's
		// standalone distribution can live below ~/.codex/packages, and the
		// Linux sandbox re-executes that binary for every tool command. A
		// whole-home deny (or an allowlist that omits it) therefore prevents
		// commands from launching at all. Path-level hardening requires a
		// measured runtime/state split and is tracked separately.
		if !rules.RequireSplitPolicy {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return "", fmt.Errorf("resolve home directory for Codex runtime grants: %w", homeErr)
			}
			fmt.Fprintf(&b, "%q = \"read\"\n", filepath.Join(home, ".codex"))
		}
	}
	if !suppressPrivateStateDeny {
		fmt.Fprintf(&b, "%q = \"none\"\n", privateStateDir)
	}
	fmt.Fprintf(&b, "%q = \"read\"\n", socketPath)
	if len(grantPaths) > 0 {
		for _, dir := range grantPaths {
			fmt.Fprintf(&b, "%q = %q\n", dir, grants[dir])
		}
		fmt.Fprintln(&b)
	} else {
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "[permissions.%s.network]\n", p)
	fmt.Fprintf(&b, "enabled = %t\n\n", networkAccess != sandboxpolicy.NetworkAccessNone || useOfflineProxy)
	fmt.Fprintf(&b, "[permissions.%s.network.unix_sockets]\n", p)
	fmt.Fprintf(&b, "%q = \"allow\"\n", socketPath)
	return b.String(), nil
}

// codexProtectedRootDenies returns the protected harness-state roots the
// managed Codex profile must deny beyond tclaude's private-state directory,
// which the caller handles separately. ~/.codex is intentionally absent: it
// can contain Codex's installed executable and must remain readable.
func codexProtectedRootDenies(privateStateDir string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for protected sandbox roots: %w", err)
	}
	path := filepath.Clean(filepath.Join(home, ".claude", "sessions"))
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = filepath.Clean(resolved)
	}
	if path == filepath.Clean(privateStateDir) {
		return nil, nil
	}
	if err := validateCodexProfilePath("protected root", path); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

// codexTmuxSocketDir returns the private directory holding tclaude's named
// tmux socket (`tmux -L tclaude`). tmux uses $TMUX_TMPDIR/tmux-UID when that
// variable is set and /tmp/tmux-UID otherwise. Blocking the directory covers
// the current socket and a server created after the profile was rendered.
//
// Windows is not a supported tclaude target; this file is built for the Linux
// and macOS targets documented by the project, where os.Getuid is available.
func codexTmuxSocketDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("TMUX_TMPDIR"))
	if base == "" {
		base = "/tmp"
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("TMUX_TMPDIR %q is not absolute", base)
	}
	base, err := filepath.EvalSymlinks(filepath.Clean(base))
	if err != nil {
		return "", fmt.Errorf("resolve tmux socket base %q: %w", base, err)
	}
	dir := filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := validateCodexProfilePath("tclaude tmux socket directory", dir); err != nil {
		return "", err
	}
	return dir, nil
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
	sock, privateStateDir, home, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	return ensureCodexAgentProfile(sock, privateStateDir, home, "")
}

// EnsureCodexAgentProfileForCwd is the spawn-time variant of
// EnsureCodexAgentProfile. It preserves the base tclaude-agent posture and, if
// cwd is inside a Git repository, adds a write grant for that repository's Git
// common dir. That is the narrow extra permission `git commit` needs from a
// linked worktree whose .git pointer targets metadata outside the workspace
// root. If cwd is not in a Git repo (or git cannot answer), the base profile is
// written.
func EnsureCodexAgentProfileForCwd(cwd string) (string, error) {
	sock, privateStateDir, home, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	gitCommonDir, err := codexGitCommonDir(cwd)
	if err != nil {
		return "", err
	}
	// Pass the real cwd (for the exact-git-dir grant, #963) and the real $HOME
	// (the home guard; privateStateDir's parent is no longer home after the
	// data/ split).
	writeDirs := GitWorktreeWriteDirs(cwd, gitCommonDir, home)
	return ensureCodexAgentProfileForWriteDirs(sock, privateStateDir, writeDirs)
}

// EnsureCodexAgentProfileForGitCommonDir is the daemon-spawn variant used
// after agentd has already resolved, proved, and pinned a Git common dir. It
// intentionally does not recompute from cwd in the forked session launcher.
func EnsureCodexAgentProfileForGitCommonDir(gitCommonDir string) (string, error) {
	sock, privateStateDir, home, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	gitCommonDir = strings.TrimSpace(gitCommonDir)
	if gitCommonDir != "" {
		gitCommonDir = filepath.Clean(gitCommonDir)
	}
	return ensureCodexAgentProfile(sock, privateStateDir, home, gitCommonDir)
}

// EnsureCodexAgentProfileForWriteDirs renders the managed profile from the
// daemon-proofed permission roots verbatim. The child must not re-derive them
// from a path the caller could mutate between verification and launch.
func EnsureCodexAgentProfileForWriteDirs(writeDirs []string) (string, error) {
	sock, privateStateDir, _, err := codexAgentSandboxPaths()
	if err != nil {
		return "", err
	}
	return ensureCodexAgentProfileForWriteDirs(sock, privateStateDir, writeDirs)
}

// EnsureCodexAgentLaunchProfile writes a launch-unique managed profile and
// returns both its Codex profile name and file path. A fixed global profile is
// unsafe for proof-scoped grants: concurrent launches for different
// repositories could overwrite one another before Codex reads the file. The
// launch ID is restricted by the normal profile-name gate and becomes part of
// both the profile name and filename.
func EnsureCodexAgentLaunchProfile(writeDirs []string, launchID string) (profileName, path string, err error) {
	return EnsureCodexAgentLaunchProfileWithGrants(nil, writeDirs, launchID)
}

// EnsureCodexAgentLaunchProfileWithGrants renders both additive read and
// write roots into a launch-unique managed profile. A write entry dominates a
// duplicate read entry.
func EnsureCodexAgentLaunchProfileWithGrants(readDirs, writeDirs []string, launchID string) (profileName, path string, err error) {
	return EnsureCodexAgentLaunchProfileWithRules(readDirs, writeDirs, nil, launchID)
}

// EnsureCodexAgentLaunchProfileWithRules renders additive read/write roots and
// restrictive deny roots into a launch-unique managed profile. Deny dominates
// an exact duplicate grant.
func EnsureCodexAgentLaunchProfileWithRules(readDirs, writeDirs, denyDirs []string, launchID string) (profileName, path string, err error) {
	return EnsureCodexAgentLaunchProfileWithRulesAndNetwork(readDirs, writeDirs, denyDirs, sandboxpolicy.NetworkAccessInherit, launchID)
}

// EnsureCodexAgentLaunchProfileWithRulesAndNetwork renders filesystem rules
// plus an optional network posture. Every managed profile also denies the
// tclaude tmux server socket while preserving agentd's separate Unix socket.
func EnsureCodexAgentLaunchProfileWithRulesAndNetwork(readDirs, writeDirs, denyDirs []string, networkAccess sandboxpolicy.NetworkAccess, launchID string) (profileName, path string, err error) {
	return EnsureCodexAgentLaunchProfileForRules(CodexSandboxRules{ReadDirs: readDirs, WriteDirs: writeDirs, DenyDirs: denyDirs}, networkAccess, launchID)
}

// EnsureCodexAgentLaunchProfileForRules is the full-fidelity entry point: it
// carries the read baseline and the acknowledged break-glass exceptions in
// addition to the ordinary filesystem rules.
func EnsureCodexAgentLaunchProfileForRules(rules CodexSandboxRules, networkAccess sandboxpolicy.NetworkAccess, launchID string) (profileName, path string, err error) {
	launchID = strings.TrimSpace(launchID)
	if launchID == "" {
		return "", "", fmt.Errorf("managed Codex launch profile requires a launch ID")
	}
	if !codexAgentLaunchIDRe.MatchString(launchID) {
		return "", "", fmt.Errorf("managed Codex launch profile ID must be 16 lowercase hex characters")
	}
	if err := CleanupStaleCodexAgentLaunchProfiles(codexAgentLaunchProfileMaxAge); err != nil {
		return "", "", err
	}
	profileName, err = ValidateCodexProfileName(CodexAgentProfile + "-" + launchID)
	if err != nil {
		return "", "", err
	}
	sock, privateStateDir, _, err := codexAgentSandboxPaths()
	if err != nil {
		return "", "", err
	}
	content, err := codexAgentProfileContentForRules(profileName, sock, privateStateDir, rules, networkAccess, runtime.GOOS)
	if err != nil {
		return "", "", err
	}
	path, err = writeCodexAgentProfile(profileName, content)
	return profileName, path, err
}

// CleanupStaleCodexAgentLaunchProfiles removes launch-unique managed profiles
// left behind by a forced tmux stop or host/process crash. Normal pane exit
// removes its own file; this age-bounded sweep prevents abnormal exits from
// accumulating proof-scoped authority files indefinitely without touching the
// installed tclaude-agent baseline or a recently started launch.
func CleanupStaleCodexAgentLaunchProfiles(maxAge time.Duration) error {
	dir, err := codexConfigDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan stale Codex launch profiles: %w", err)
	}
	cutoff := time.Now().Add(-maxAge)
	var failures []string
	for _, entry := range entries {
		name := entry.Name()
		if !codexAgentLaunchProfileFileRe.MatchString(name) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			failures = append(failures, name+": "+infoErr.Error())
			continue
		}
		if maxAge > 0 && info.ModTime().After(cutoff) {
			continue
		}
		if removeErr := os.Remove(filepath.Join(dir, name)); removeErr != nil && !os.IsNotExist(removeErr) {
			failures = append(failures, name+": "+removeErr.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("remove stale Codex launch profiles: %s", strings.Join(failures, "; "))
	}
	return nil
}

// CodexGitCommonDir resolves the Git common dir for cwd. Daemon spawn paths use
// this before dir write-proof verification so linked-worktree metadata grants
// are proven and pinned instead of being recomputed after launch.
func CodexGitCommonDir(cwd string) (string, error) {
	return GitCommonDir(cwd)
}

// codexAgentSandboxPaths resolves the paths the managed Codex profile pins: the
// agent-reachable agentd socket (allowlisted), the private-state dir
// ~/.tclaude/data (denied), and the real $HOME. home is returned explicitly —
// rather than derived from privateStateDir — because it is the home guard for
// GitWorktreeWriteDirs, and privateStateDir is now two levels below home
// (~/.tclaude/data), so filepath.Dir(privateStateDir) is ~/.tclaude, not home.
func codexAgentSandboxPaths() (socketPath, privateStateDir, homeDir string, err error) {
	socketPath = agentipc.CanonicalSocketPath()
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", err
	}
	return socketPath, filepath.Join(home, ".tclaude", "data"), home, nil
}

// ensureCodexAgentProfile is the socket-path-injected core of
// EnsureCodexAgentProfile, split out so tests can drive it without depending
// on the caller's $HOME layout.
func ensureCodexAgentProfile(socketPath, privateStateDir, homeDir, gitCommonDir string) (string, error) {
	// Base (no-cwd) path: no exact-git-dir grant; homeDir is the real $HOME.
	writeDirs := GitWorktreeWriteDirs("", gitCommonDir, homeDir)
	return ensureCodexAgentProfileForWriteDirs(socketPath, privateStateDir, writeDirs)
}

func ensureCodexAgentProfileForWriteDirs(socketPath, privateStateDir string, writeDirs []string) (string, error) {
	return ensureCodexAgentProfileForWriteDirsNamed(CodexAgentProfile, socketPath, privateStateDir, writeDirs)
}

func ensureCodexAgentProfileForWriteDirsNamed(profileName, socketPath, privateStateDir string, writeDirs []string) (string, error) {
	return ensureCodexAgentProfileForGrantsNamed(profileName, socketPath, privateStateDir, nil, writeDirs)
}

func ensureCodexAgentProfileForGrantsNamed(profileName, socketPath, privateStateDir string, readDirs, writeDirs []string) (string, error) {
	return ensureCodexAgentProfileForRulesNamed(profileName, socketPath, privateStateDir, readDirs, writeDirs, nil)
}

func ensureCodexAgentProfileForRulesNamed(profileName, socketPath, privateStateDir string, readDirs, writeDirs, denyDirs []string) (string, error) {
	return ensureCodexAgentProfileForRulesAndNetworkNamed(profileName, socketPath, privateStateDir, readDirs, writeDirs, denyDirs, sandboxpolicy.NetworkAccessInherit)
}

func ensureCodexAgentProfileForRulesAndNetworkNamed(profileName, socketPath, privateStateDir string, readDirs, writeDirs, denyDirs []string, networkAccess sandboxpolicy.NetworkAccess) (string, error) {
	content, err := codexAgentProfileContentForNameAndRulesAndNetwork(profileName, socketPath, privateStateDir, readDirs, writeDirs, denyDirs, networkAccess)
	if err != nil {
		return "", err
	}
	return writeCodexAgentProfile(profileName, content)
}

func writeCodexAgentProfile(profileName, content string) (string, error) {
	path, err := codexAgentProfilePath(profileName)
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
	sock, privateStateDir, home, err := codexAgentSandboxPaths()
	if err != nil {
		return "", false, false, err
	}
	baseWant, err := codexAgentProfileContent(sock, privateStateDir, home, "")
	if err != nil {
		return "", false, false, err
	}
	var acceptable []string
	acceptable = append(acceptable, baseWant)
	if cwd, gerr := os.Getwd(); gerr == nil {
		if gitCommonDir, gerr := codexGitCommonDir(cwd); gerr == nil && gitCommonDir != "" {
			if want, gerr := codexAgentProfileContent(sock, privateStateDir, home, gitCommonDir); gerr == nil {
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
