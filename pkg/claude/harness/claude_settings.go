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
	if block := claudeSandboxBlock(spec.SandboxMode); block != nil {
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
		allowWrite := make([]any, len(dirs))
		for i, dir := range dirs {
			allowWrite[i] = dir
		}
		filesystem["allowWrite"] = allowWrite
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
