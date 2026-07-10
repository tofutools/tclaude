package harness

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Codex sandbox modes — openai/codex `SandboxMode` (kebab-case), verified
// firsthand against rust-v0.139.0. workspace-write writes only cwd + /tmp +
// $TMPDIR ($HOME read-only)
// with network denied: the secure default for a tclaude-spawned agent.
// read-only adds no writes; danger-full-access disables the sandbox and is
// never a default — explicit opt-in only.
const (
	SandboxReadOnly       = "read-only"
	SandboxWorkspaceWrite = "workspace-write"
	SandboxDangerFull     = "danger-full-access"
)

// SandboxManagedProfile is a spawn-UI *pseudo-mode* — NOT one of Codex's
// real `--sandbox` modes. It selects tclaude's managed permission profile
// (`codex -p tclaude-agent`) instead of a raw `--sandbox` flag, and is the
// recommended default for a daemon-spawned Codex agent: unlike every real
// `--sandbox` mode (which makes Codex ignore permission profiles, including
// the Unix-socket allowlist), the profile gives the same workspace-write
// containment AND keeps the agentd socket reachable, so the agent can still
// run `tclaude agent …` while sandboxed (JOH-207).
//
// Its value is deliberately the profile name (CodexAgentProfile): the
// dashboard sandbox dropdown carries it verbatim, the spawn boundary
// (appendSandboxArgs) translates it to `--permission-profile`, and a direct
// `tclaude session new --sandbox tclaude-agent` is normalized to the same
// profile rather than emitted as a bogus literal `--sandbox` value. It is
// never passed to Codex's `--sandbox` flag.
const SandboxManagedProfile = CodexAgentProfile

// codexSandbox is Codex's SandboxCatalog. The default is the managed
// permission profile (SandboxManagedProfile) — workspace-write containment
// (cwd + /tmp + $TMPDIR writable, $HOME read-only, network denied) plus the
// agentd-socket allowlist — so a tclaude-spawned Codex agent gets the
// guardrail-integrity property *and* stays able to coordinate via `tclaude
// agent`. The three raw `--sandbox` modes remain selectable for callers that
// want Codex's native containment without the managed profile.
type codexSandbox struct{}

func (codexSandbox) DefaultMode() string { return SandboxManagedProfile }

// Modes lists the launch-containment options for spawn UIs: the recommended
// managed profile first (the spawn default), then Codex's three raw
// `--sandbox` modes. A fresh slice each call so a caller can't mutate the
// set.
func (codexSandbox) Modes() []string {
	return []string{SandboxManagedProfile, SandboxWorkspaceWrite, SandboxReadOnly, SandboxDangerFull}
}

// codexSandboxModeHelp is the one-line description the spawn UI shows for each
// selectable mode, calling out agentd-socket reachability — the property that
// surprised operators (the raw `--sandbox` modes make Codex ignore permission
// profiles, blocking the socket, so the agent can't run `tclaude agent …`). The
// leading "⚠" marks the modes the dialog should flag. Keyed by mode value.
var codexSandboxModeHelp = map[string]string{
	SandboxManagedProfile: "Recommended. Workspace-write containment (only the working directory is writable; ~/.tclaude is inaccessible) PLUS access to agentd's state-free socket — the agent CAN run `tclaude agent` (coordinate, reincarnate, notify-human).",
	SandboxWorkspaceWrite: "Raw Codex sandbox — only the working directory is writable ($HOME read-only). ⚠ No agentd access: the agent CANNOT run `tclaude agent`.",
	SandboxReadOnly:       "Raw Codex sandbox — no filesystem writes at all. ⚠ No agentd access: the agent CANNOT run `tclaude agent`.",
	SandboxDangerFull:     "⚠ Sandbox OFF — full read/write access to your machine (the agent can run `tclaude agent`). Explicit opt-in only.",
}

// ModeHelp returns a one-line description of a sandbox mode for spawn UIs, or
// "" for an unrecognized mode.
func (codexSandbox) ModeHelp(mode string) string {
	return codexSandboxModeHelp[strings.TrimSpace(mode)]
}

func (codexSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", SandboxManagedProfile, SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFull:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid codex sandbox mode %q (want %s|%s|%s|%s)",
			mode, SandboxManagedProfile, SandboxWorkspaceWrite, SandboxReadOnly, SandboxDangerFull)
	}
}

// CodexSandboxCwdConflict reports whether spawning a Codex agent at cwd
// under the given (already-validated) sandbox mode would expose tclaude's
// daemon-state dirs (~/.tclaude, ~/.codex, ~/.claude/sessions) to the
// agent's own writes — defeating the protection the sandbox is supposed to
// provide.
//
// It is true only for the *writable* sandboxed modes — workspace-write and
// the managed profile (SandboxManagedProfile, which extends :workspace, the
// same cwd-subtree writability) — when cwd is at or above one of those
// protected dirs: their writable root is the cwd subtree, so a cwd that
// contains a protected dir makes it writable. read-only can't write;
// danger-full-access is the explicit no-sandbox opt-out (the caller already
// accepted full access), so neither conflicts. The spawn boundary calls this
// with the resolved, absolute cwd and home (os.UserHomeDir()); a cwd strictly
// *inside* a normal project dir (e.g. ~/projects/foo) never conflicts.
//
// Both cwd and home are passed through filepath.EvalSymlinks first, because
// Codex confines writes to the *resolved* real path: a cwd like
// /tmp/link -> /home/dev would otherwise slip past a textual Rel comparison
// yet leave $HOME writable. EvalSymlinks failures (e.g. a not-yet-created
// path) fall back to the cleaned path rather than skipping the guard — the
// check stays fail-closed.
func CodexSandboxCwdConflict(mode, cwd, home string) bool {
	if (mode != SandboxWorkspaceWrite && mode != SandboxManagedProfile) || cwd == "" || home == "" {
		return false
	}
	cwd, home = resolveSymlinks(cwd), resolveSymlinks(home)
	for _, sub := range codexProtectedSubdirs {
		if pathContainsOrEqual(cwd, filepath.Join(home, sub)) {
			return true
		}
	}
	return false
}

// resolveSymlinks returns p with its longest *existing* ancestor
// symlink-resolved and the non-existent remainder re-attached. Resolving
// the existing prefix (rather than only the whole path) is what makes the
// guard correct: Codex confines writes to the resolved real path, and two
// paths that share an existing ancestor — a cwd and a $HOME under the same
// root — must resolve that ancestor *identically* for the ancestor check to
// hold. EvalSymlinks on the whole path alone fails the moment any leaf is
// synthetic (a not-yet-created cwd, or a platform autofs mount like macOS
// /home where the parent resolves but the child doesn't), leaving cwd and
// home in divergent trees and silently dropping the guard. A path with no
// resolvable ancestor falls back to filepath.Clean(p) — never skips the
// guard. Mirrors the tolerant intent of worktree.sameDir.
func resolveSymlinks(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without resolving anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// codexProtectedSubdirs are the $HOME-relative trees that must stay
// unwritable by a sandboxed agent — the daemon state + identity files
// docs/sandbox-hardening.md names, plus Codex's own config/state home
// (~/.codex holds hooks.json + state_5.sqlite + the rollout tree).
var codexProtectedSubdirs = []string{
	".tclaude",
	".codex",
	filepath.Join(".claude", "sessions"),
}

// pathContainsOrEqual reports whether dir is the same path as, or an
// ancestor of, target. Both should be absolute + cleaned. It compares by
// path segments via filepath.Rel, so it is not fooled by shared string
// prefixes (e.g. /home/foo vs /home/foobar).
func pathContainsOrEqual(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true // same path
	}
	// target is under dir iff getting from dir to target never steps up
	// ("..") — i.e. rel is neither ".." nor a "../…" path.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
