package harness

import (
	"encoding/json"
	"path/filepath"
	"strings"
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
	// The block builder is given the acknowledged break-glass paths so it can
	// drop exactly the protected denies those paths reopen.
	breakGlass := append(append([]string{}, spec.SandboxBreakGlassReadDirs...), spec.SandboxBreakGlassWriteDirs...)
	if block := claudeSandboxBlockWithBreakGlass(spec.SandboxMode, breakGlass); block != nil {
		settings["sandbox"] = block
	}
	if dirs := normalizedSandboxWriteDirs(spec.SandboxWriteDirs); len(dirs) > 0 &&
		strings.TrimSpace(spec.SandboxMode) != ClaudeSandboxOff {
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
	if dirs := normalizedSandboxWriteDirs(spec.SandboxReadDirs); len(dirs) > 0 &&
		strings.TrimSpace(spec.SandboxMode) != ClaudeSandboxOff {
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
	if dirs := normalizedSandboxWriteDirs(spec.SandboxDenyDirs); len(dirs) > 0 &&
		strings.TrimSpace(spec.SandboxMode) != ClaudeSandboxOff {
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
	}
	// Break-glass rides the SAME allowRead/allowWrite keys as ordinary grants,
	// because that is exactly Claude's documented re-open mechanism ("Paths to
	// re-allow reading within denyRead regions. Takes precedence over
	// denyRead"). What makes it work is the paired suppression in
	// claudeSandboxBlock: the protected deny that would otherwise mask the
	// grant is not emitted for a path an operator explicitly acknowledged.
	// Read is applied before write and never implies it.
	if dirs := normalizedSandboxWriteDirs(spec.SandboxBreakGlassReadDirs); len(dirs) > 0 &&
		strings.TrimSpace(spec.SandboxMode) != ClaudeSandboxOff {
		appendSandboxFilesystemDirs(claudeSandboxFilesystem(settings), "allowRead", dirs)
	}
	if dirs := normalizedSandboxWriteDirs(spec.SandboxBreakGlassWriteDirs); len(dirs) > 0 &&
		strings.TrimSpace(spec.SandboxMode) != ClaudeSandboxOff {
		filesystem := claudeSandboxFilesystem(settings)
		// Claude's write policy is allowlist-shaped, so a protected write needs
		// allowWrite; it also needs allowRead, since a path that cannot be read
		// cannot usefully be written by a tool that opens it first. That is a
		// consequence of granting WRITE, never of granting read.
		appendSandboxFilesystemDirs(filesystem, "allowWrite", dirs)
		appendSandboxFilesystemDirs(filesystem, "allowRead", dirs)
	}
	if v := claudeAskTimeoutValue(spec.AskUserQuestionTimeout); v != "" {
		settings["askUserQuestionTimeout"] = v
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

// claudeSandboxFilesystem lazily creates settings["sandbox"]["filesystem"],
// matching the inherit-safe behavior above: an inherit/unset launch omits
// `enabled`, so the filesystem array merges with the operator's settings and
// matters only when their sandbox is enabled.
func claudeSandboxFilesystem(settings map[string]any) map[string]any {
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
	return filesystem
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
