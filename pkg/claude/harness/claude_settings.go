package harness

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// claudeSettingsJSON collects every per-session Claude Code settings.json
// override a spawn carries into ONE compact `--settings` payload, or "" when
// nothing is overridden (the spawner then omits the flag and the agent runs on
// the operator's own settings.json).
//
// Claude Code emits no launch flag for these settings — the per-session lever is
// `claude --settings '<json>'`, which merges a block over the user/project files
// (only managed/policy settings outrank it). Because the spawner emits
// `--settings` AT MOST ONCE, every override source (the OS sandbox block, the
// AskUserQuestion idle-timeout, and any future settings.json key tclaude learns
// to override per-agent) must share this single merged object rather than each
// appending its own flag. Adding a new override is therefore a one-line addition
// here plus its own catalog file — this is the general seam.
//
// json.Marshal sorts map keys, so the output is deterministic (testable).
func claudeSettingsJSON(spec SpawnSpec) string {
	settings := map[string]any{}
	if block := claudeSandboxBlock(spec.HarnessBuiltinMode); block != nil {
		settings["sandbox"] = block
	}
	if dirs := withoutCanonicalAgentdDir(normalizedSandboxWriteDirs(spec.SandboxWriteDirs)); len(dirs) > 0 &&
		strings.TrimSpace(spec.HarnessBuiltinMode) != ClaudeSandboxOff {
		block, _ := settings["sandbox"].(map[string]any)
		if block == nil {
			// An inherit/unset launch intentionally omits enabled: the filesystem
			// array merges with the operator's settings and matters only when
			// their sandbox is enabled.
			block = map[string]any{}
			settings["sandbox"] = block
		}
		filesystem, _ := block["filesystem"].(map[string]any)
		if filesystem == nil {
			filesystem = map[string]any{}
			block["filesystem"] = filesystem
		}
		appendSandboxFilesystemDirs(filesystem, "allowWrite", dirs)
	}
	if dirs := withoutCanonicalAgentdDir(normalizedSandboxWriteDirs(spec.SandboxReadDirs)); len(dirs) > 0 &&
		strings.TrimSpace(spec.HarnessBuiltinMode) != ClaudeSandboxOff {
		block, _ := settings["sandbox"].(map[string]any)
		if block == nil {
			block = map[string]any{}
			settings["sandbox"] = block
		}
		filesystem, _ := block["filesystem"].(map[string]any)
		if filesystem == nil {
			filesystem = map[string]any{}
			block["filesystem"] = filesystem
		}
		appendSandboxFilesystemDirs(filesystem, "allowRead", dirs)
	}
	if dirs := withoutCanonicalAgentdDir(normalizedSandboxWriteDirs(spec.SandboxDenyDirs)); len(dirs) > 0 &&
		strings.TrimSpace(spec.HarnessBuiltinMode) != ClaudeSandboxOff {
		block, _ := settings["sandbox"].(map[string]any)
		if block == nil {
			block = map[string]any{}
			settings["sandbox"] = block
		}
		filesystem, _ := block["filesystem"].(map[string]any)
		if filesystem == nil {
			filesystem = map[string]any{}
			block["filesystem"] = filesystem
		}
		appendSandboxFilesystemDirs(filesystem, "denyRead", dirs)
		appendSandboxFilesystemDirs(filesystem, "denyWrite", dirs)
		// Mirror the same denies onto the tool-permission surface so one
		// authored row binds the built-in Read/Write/Edit tools too, not just
		// Bash (TCL-666).
		rules, skipped := claudeToolPermissionDenyRules(
			spec.SandboxReadDirs, spec.SandboxWriteDirs, dirs,
		)
		appendClaudePermissionDeny(settings, rules)
		if len(skipped) > 0 {
			// Not silent: these denies bind Bash but NOT the built-in file
			// tools, and nothing else in the launch says so.
			slog.Warn("claude sandbox: deny rows enforced for Bash only, not the built-in file tools",
				"paths", skipped,
				"reason", "a reopen beneath the deny cannot be expressed as a permission rule (deny precedes allow)")
		}
	}
	if strings.TrimSpace(spec.HarnessBuiltinMode) != ClaudeSandboxOff {
		appendClaudeProtectedRootDenies(settings, spec.SandboxReadDirs, spec.SandboxWriteDirs)
	}
	if v := claudeAskTimeoutValue(spec.AskUserQuestionTimeout); v != "" {
		settings["askUserQuestionTimeout"] = v
	}
	// Startup-context trims with no CLAUDE_CODE_DISABLE_* twin (TCL-597). The
	// env-backed majority of the catalog rides ApplyContextFeaturesEnv instead;
	// only these need the settings payload, and they join the shared object here
	// rather than growing a second `--settings` flag.
	for key, disabled := range ContextFeatureSettings(spec.ContextFeatures) {
		settings[key] = disabled
	}
	// Claude Code's own cross-session messaging mesh (TCL-812). OFF — the
	// default — writes the inbound refusal, the cross-machine approval
	// requirement, and the ListAgents deny; ON writes nothing. See
	// peer_messaging.go for why the opt-in direction is silent, and why the deny
	// names ListAgents rather than SendMessage.
	if keys, denyRules := PeerMessagingSettings(spec.PeerMessaging); len(keys) > 0 {
		for key, value := range keys {
			settings[key] = value
		}
		appendClaudePermissionDeny(settings, denyRules)
	}
	if len(settings) == 0 {
		return ""
	}
	b, err := json.Marshal(settings)
	if err != nil {
		// Unreachable for these static/enum values; never emit half-built JSON.
		return ""
	}
	return string(b)
}

// appendClaudeProtectedRootDenies re-denies every protected root a read/write
// grant covers. Profile validation admits a grant on an ancestor of a protected
// root (a writable `~`), relying on each applier to restore the wall on top of
// it. Only the `on` block carries these denies unconditionally, so an inherit
// launch over an operator settings.json without the hardening block would
// otherwise hand the ancestor's access straight through. Nothing reopens
// beneath a protected root, so the permission mirror is always expressible.
func appendClaudeProtectedRootDenies(settings map[string]any, readDirs, writeDirs []string) {
	grants := append(normalizedSandboxWriteDirs(readDirs), normalizedSandboxWriteDirs(writeDirs)...)
	if len(grants) == 0 {
		return
	}
	var covered, patterns []string
	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		// Fail closed: without a resolved Home no grant can be proven not to
		// cover a protected root, so deny both in the spelling the `on` block
		// uses, which Claude Code expands itself.
		slog.Warn("claude sandbox: could not resolve protected roots; denying them by ~ spelling", "error", err)
		covered = []string{tclaudePrivateStateDirTilde, tclaudeClaudeSessionsDirTilde}
		for _, root := range covered {
			patterns = append(patterns, root+"/**")
		}
	} else {
		for _, root := range protected {
			for _, dir := range grants {
				if sandboxpolicy.GuardContainsOrEqual(dir, root) {
					covered = append(covered, root)
					patterns = append(patterns, claudeAbsolutePermissionPattern(root))
					break
				}
			}
		}
	}
	if len(covered) == 0 {
		return
	}
	block, _ := settings["sandbox"].(map[string]any)
	if block == nil {
		block = map[string]any{}
		settings["sandbox"] = block
	}
	filesystem, _ := block["filesystem"].(map[string]any)
	if filesystem == nil {
		filesystem = map[string]any{}
		block["filesystem"] = filesystem
	}
	appendSandboxFilesystemDirs(filesystem, "denyRead", covered)
	appendSandboxFilesystemDirs(filesystem, "denyWrite", covered)
	rules := make([]string, 0, 2*len(patterns))
	for _, pattern := range patterns {
		rules = append(rules, "Read("+pattern+")", "Edit("+pattern+")")
	}
	appendClaudePermissionDeny(settings, rules)
}

func appendSandboxFilesystemDirs(filesystem map[string]any, key string, dirs []string) {
	existing, _ := filesystem[key].([]any)
	seen := make(map[string]bool, len(existing)+len(dirs))
	out := make([]any, 0, len(existing)+len(dirs))
	for _, value := range existing {
		path, ok := value.(string)
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, path := range dirs {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	filesystem[key] = out
}

func normalizedSandboxWriteDirs(dirs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		dir = filepath.Clean(strings.TrimSpace(dir))
		if dir == "." || !filepath.IsAbs(dir) || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

func withoutCanonicalAgentdDir(dirs []string) []string {
	canonical := filepath.Clean(agentipc.CanonicalSocketDir())
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if canonical != "." && filepath.Clean(dir) == canonical {
			continue
		}
		out = append(out, dir)
	}
	return out
}
