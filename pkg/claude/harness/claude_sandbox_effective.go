package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The unsandboxed-autonomy check (TCL-586).
//
// tclaude's approval contract (see ApprovalCatalog in approval.go and
// approvalAutoCommands in approval_lineage.go) says an unattended agent may run
// arbitrary commands automatically *while they stay inside the agent's
// sandbox*. For Codex that pairing is enforced by tclaude itself: the spawn
// default is the managed permission profile. For Claude Code it is not. Claude's
// sandbox default is `inherit`, which deliberately emits no `--settings`
// override so the operator's own settings.json posture survives — and if that
// posture configures no sandbox, a default Claude spawn runs `auto` with the
// supervisor classifier as the only gate.
//
// The operator decision (TCL-586) was NOT to force sandbox `on` for Claude —
// that would override settings.json on an axis the sandbox default deliberately
// leaves alone. Instead tclaude *tells the operator* when it is about to launch
// that combination, on the CLI and in the dashboard spawn dialog. Saying so
// requires knowing whether the sandbox will actually be active, which under
// `inherit` means inspecting the settings files Claude Code itself will read —
// hence ResolveClaudeSandboxEnabled below, rather than a guess from the mode
// token alone.

// ClaudeSandboxState is the tri-state answer to "will Claude Code's OS sandbox
// be active for this launch". Unknown is a real, distinct outcome: no settings
// file tclaude can read says anything about `sandbox.enabled`, so tclaude knows
// only that nothing turned it on. It is reported separately from Off so the
// operator-facing copy can say "nothing configures it" rather than claiming a
// file disabled it.
type ClaudeSandboxState int

const (
	ClaudeSandboxStateUnknown ClaudeSandboxState = iota
	ClaudeSandboxStateOn
	ClaudeSandboxStateOff
)

// Active reports whether the sandbox is known to be on. Unknown counts as not
// active: Claude Code's sandbox is opt-in, so an unconfigured launch runs
// unconfined.
func (s ClaudeSandboxState) Active() bool { return s == ClaudeSandboxStateOn }

func (s ClaudeSandboxState) String() string {
	switch s {
	case ClaudeSandboxStateOn:
		return "on"
	case ClaudeSandboxStateOff:
		return "off"
	default:
		return "unconfigured"
	}
}

// ClaudeSandboxResolution is what tclaude could determine about a launch's OS
// sandbox, plus where it learned it.
type ClaudeSandboxResolution struct {
	// State is the effective answer for this launch.
	State ClaudeSandboxState
	// Source names the thing that decided State — the launch itself for an
	// explicit on/off, otherwise the settings file whose `sandbox.enabled` won
	// the precedence chain. Empty when nothing decided (State Unknown).
	Source string
	// Diagnostics record settings files that exist but could not be read or
	// parsed. They are surfaced beside the verdict rather than swallowed,
	// because an unreadable file is precisely the case where the verdict may be
	// wrong. They never contain parser excerpts: a settings file can hold
	// secrets outside the sandbox block and this value reaches agent-readable
	// surfaces.
	Diagnostics []string
}

// ResolveClaudeSandboxEnabled answers whether Claude Code's OS sandbox will be
// active for a launch with the given (already validated) tclaude sandbox mode,
// starting the project-settings search at cwd.
//
//   - `on` / `off`: tclaude emits a `--settings` sandbox block, which outranks
//     every user/project file, so the mode alone is the answer.
//   - `inherit` (the default) or unset: tclaude emits nothing, so the answer is
//     whatever the operator's own settings say. That is read here.
//
// A missing file is not an error — it simply says nothing. Only managed policy
// settings outrank a `--settings` block, so `on`/`off` are reported without
// consulting them; that is a deliberate simplification, since a managed policy
// that pins `sandbox.enabled` overrides tclaude on every axis anyway.
func ResolveClaudeSandboxEnabled(mode, cwd string) ClaudeSandboxResolution {
	switch strings.TrimSpace(mode) {
	case ClaudeSandboxOn:
		return ClaudeSandboxResolution{State: ClaudeSandboxStateOn, Source: "this launch (sandbox `on`)"}
	case ClaudeSandboxOff:
		return ClaudeSandboxResolution{State: ClaudeSandboxStateOff, Source: "this launch (sandbox `off`)"}
	}
	var out ClaudeSandboxResolution
	for _, path := range claudeSettingsPrecedence(cwd) {
		enabled, found, diagnostic := readClaudeSandboxEnabled(path)
		if diagnostic != "" {
			out.Diagnostics = append(out.Diagnostics, diagnostic)
			continue
		}
		if !found {
			continue
		}
		out.Source = displayClaudeSettingsPath(path)
		if enabled {
			out.State = ClaudeSandboxStateOn
		} else {
			out.State = ClaudeSandboxStateOff
		}
		return out
	}
	return out
}

// claudeSettingsPrecedence lists the settings files Claude Code consults, most
// authoritative first, so the first one that specifies `sandbox.enabled` decides:
//
//  1. enterprise managed policy settings (and its drop-in directory);
//  2. project `.claude/settings.local.json`, then `.claude/settings.json`;
//  3. the user's `~/.claude/settings.json`.
//
// Claude Code discovers the project `.claude` directory by walking up from the
// launch directory, so this walks cwd's ancestors nearest-first. The walk stops
// AT the home directory rather than continuing into it: `~/.claude` is the user
// tier, and letting it also answer as a project tier would silently promote it
// above a real project's settings for any repo that happens to live under home
// — which is most of them.
//
// Paths are returned unfiltered (existence is checked by the reader) so the
// order stays a plain, testable statement of precedence.
func claudeSettingsPrecedence(cwd string) []string {
	home, homeErr := os.UserHomeDir()
	stop := ""
	if homeErr == nil {
		stop = home
	}
	paths := make([]string, 0, 8)
	paths = append(paths, claudeManagedSettingsPaths()...)
	for _, dir := range ancestorDirs(cwd, stop) {
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.local.json"),
			filepath.Join(dir, ".claude", "settings.json"))
	}
	if homeErr == nil {
		paths = append(paths, filepath.Join(home, ".claude", "settings.json"))
	}
	return paths
}

// claudeManagedSettingsRoot names the directory holding the enterprise
// managed-policy settings for this platform. It is a variable so a test can
// point it at a temp dir: the real path is machine-global and may genuinely
// exist on a developer or CI host, which would otherwise make these tests pass
// or fail depending on whose laptop they run on.
var claudeManagedSettingsRoot = func() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode"
	}
	return "/etc/claude-code"
}

// claudeManagedSettingsPaths returns the enterprise managed-policy settings
// file for this platform plus any drop-ins beside it, in lexical order. Managed
// settings outrank everything including a CLI `--settings` block, so they lead
// the chain. An unreadable drop-in directory is silently skipped: it is an
// administrator-owned path an ordinary operator cannot inspect anyway, and the
// resolution already degrades to "unknown" (which warns) rather than to a
// false all-clear.
func claudeManagedSettingsPaths() []string {
	root := claudeManagedSettingsRoot()
	paths := []string{filepath.Join(root, "managed-settings.json")}
	entries, err := os.ReadDir(filepath.Join(root, "managed-settings.d"))
	if err != nil {
		return paths
	}
	dropIns := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		dropIns = append(dropIns, filepath.Join(root, "managed-settings.d", entry.Name()))
	}
	sort.Strings(dropIns)
	return append(paths, dropIns...)
}

// ancestorDirs returns dir and each of its parents, nearest first, stopping
// before stop (when dir is under it) or at the filesystem root. A relative or
// empty cwd yields nothing rather than a walk rooted at ".": the caller's
// launch directory is always absolute by the time a spawn resolves, and
// guessing from a relative path would inspect files of whatever process
// happened to be running.
func ancestorDirs(dir, stop string) []string {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || !filepath.IsAbs(dir) {
		return nil
	}
	stop = filepath.Clean(strings.TrimSpace(stop))
	out := make([]string, 0, 8)
	for {
		if stop != "." && dir == stop {
			return out
		}
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			return out
		}
		dir = parent
	}
}

// readClaudeSandboxEnabled reports the `sandbox.enabled` value in one settings
// file. found is false when the file is absent or simply does not mention the
// key — both mean "this tier says nothing", which is what lets the caller fall
// through to the next tier.
func readClaudeSandboxEnabled(path string) (enabled, found bool, diagnostic string) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, false, ""
	}
	if err != nil {
		return false, false, fmt.Sprintf("Could not read %s, so tclaude cannot tell whether it enables the Claude Code sandbox.", displayClaudeSettingsPath(path))
	}
	var settings struct {
		Sandbox struct {
			Enabled *bool `json:"enabled"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		// Never echo the parser diagnostic: settings.json can carry secrets
		// outside the sandbox block and this string reaches agent-readable
		// surfaces (spawn responses, the dashboard).
		return false, false, fmt.Sprintf("Could not parse %s (not valid JSON), so tclaude cannot tell whether it enables the Claude Code sandbox.", displayClaudeSettingsPath(path))
	}
	if settings.Sandbox.Enabled == nil {
		return false, false, ""
	}
	return *settings.Sandbox.Enabled, true, ""
}

// displayClaudeSettingsPath abbreviates a path under the operator's home to
// `~/…`, matching how the sandbox docs and dialog copy spell these files.
func displayClaudeSettingsPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.Join("~", rel)
}

// claudeApprovalRunsCommandsUnattended reports whether a resolved Claude
// permission mode may run arbitrary commands with no human in the loop. It
// reads the answer off the capability lattice rather than restating a mode
// list, so a future mode is classified once, in classifyApprovalLineage, and
// this warning follows automatically.
//
// acceptEdits deliberately does NOT qualify: it holds approvalAutoEdits only,
// so its unattended writes stay in the working directory. `inherit` does not
// qualify either — as a parent posture the lattice credits it with the baseline
// alone, and warning about an unknowable posture would fire on every launch
// that asked for exactly the operator's own settings.
func claudeApprovalRunsCommandsUnattended(policy string) bool {
	posture := classifyApprovalLineage(DefaultName, policy, false, false)
	return posture.valid && posture.capability&approvalAutoCommands != 0
}

// UnsandboxedAutonomyWarnings returns the operator-facing lines for a launch
// that pairs an unattended command-running permission mode with an OS sandbox
// tclaude cannot prove is active, or nil when the pairing is sound (or not
// applicable to this harness). The warning comes first; any settings file
// tclaude could not inspect follows as its own line, so an operator sees why a
// verdict might be incomplete instead of trusting a silent all-clear.
//
// It is the single entry point every surface uses — the HTTP group-spawn
// endpoint, template/wave deploys, `tclaude session new`, and the dashboard's
// effective-posture probe — so all of them say the same sentence for the same
// inputs.
//
// approvalPolicy and sandboxMode must be the FINAL resolved values — after
// profile overlay and after ResolveApprovalPolicy / ResolveSandboxMode have
// applied harness defaults. Warning on a pre-default value would either miss
// the default `auto` spawn (the case TCL-586 is about) or invent a warning for
// a mode the operator never gets. cwd is the launch directory, used to find
// project-level settings; an empty cwd simply narrows the search to the user
// and managed tiers.
//
// Only Claude Code can reach this state: Codex's spawn default is the managed
// permission profile, so its autonomy and its sandbox are resolved together.
func UnsandboxedAutonomyWarnings(h *Harness, approvalPolicy, sandboxMode, cwd string) []string {
	if h == nil || normalizeLineageHarness(h.Name) != DefaultName {
		return nil
	}
	if !claudeApprovalRunsCommandsUnattended(approvalPolicy) {
		return nil
	}
	resolution := ResolveClaudeSandboxEnabled(sandboxMode, cwd)
	if resolution.State.Active() {
		// Still surface diagnostics: a higher-precedence file tclaude could not
		// read is exactly the case where "you are sandboxed" may be wrong.
		return resolution.Diagnostics
	}
	return append([]string{claudeUnsandboxedAutonomyMessage(approvalPolicy, sandboxMode, resolution)},
		resolution.Diagnostics...)
}

// claudeUnsandboxedAutonomyMessage renders the warning copy. It names what the
// agent may do, why tclaude believes nothing confines it (with the deciding
// file, when there is one), and the two ways to fix it — because a warning an
// operator cannot act on is noise they will learn to skip.
func claudeUnsandboxedAutonomyMessage(policy, mode string, resolution ClaudeSandboxResolution) string {
	var because string
	switch {
	// The launch's own `off` is checked first: it is the reason the operator can
	// actually change from this dialog, and it outranks whatever their settings
	// say anyway.
	case strings.TrimSpace(mode) == ClaudeSandboxOff:
		because = "but this launch forces the OS sandbox off"
	case resolution.State == ClaudeSandboxStateOff && resolution.Source != "":
		because = fmt.Sprintf("but the OS sandbox is turned off by %s", resolution.Source)
	default:
		because = "but no Claude Code settings file tclaude can see enables the OS sandbox"
	}
	// bypassPermissions has no classifier at all, so naming one would understate
	// the exposure by exactly the guardrail that is missing.
	remaining := "the supervisor classifier is the only thing between it and your whole machine"
	if strings.TrimSpace(policy) == claudePermBypass {
		remaining = "nothing at all stands between it and your whole machine"
	}
	return fmt.Sprintf(
		"⚠ permission mode %q lets this agent run commands unattended, %s, so %s. "+
			"Spawn with sandbox %q to confine it, or run `tclaude setup --install-sandbox-hardening` "+
			"to enable Claude Code's sandbox globally.",
		strings.TrimSpace(policy), because, remaining, ClaudeSandboxOn)
}
