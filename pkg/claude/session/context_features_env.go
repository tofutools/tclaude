package session

import (
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyContextFeaturesEnv injects the env-backed half of a resolved
// startup-context trim map into the spawned/resumed pane's environment
// (TCL-597).
//
// Why tclaude steers this at all: Claude Code loads a large, fixed body of
// startup context — bundled skills, tool schemas, system-prompt blocks — sized
// for a general-purpose assistant. A tclaude worker agent is usually spawned for
// one narrow job, and every capability it will never use is context it must
// still read past. Trimming raises the share of its window that describes the
// actual task, which is the whole point of the feature: fewer distractions, less
// context rot, more focused agents.
//
// The map is authoritative and sparse: only slugs the operator explicitly set to
// on/off appear, so an untouched feature keeps Claude Code's own default and the
// operator's own environment. See harness.ContextFeatureEnv for why ON writes an
// empty value instead of omitting the variable.
//
// A no-op for any harness with no steerable startup-context surface (Codex,
// OpenCode) and for an empty map, so the call sites stay simple. It is the
// single seam both env-assembly paths route through — session.runNew (spawn and
// `tclaude session new -r` resume) and conv.resumeLaunchCmd (watch-mode resume)
// — the sibling of ApplyAutoMemoryEnv and ApplyClaudeResumeEnv.
//
// Settings-only trims (those with no CLAUDE_CODE_DISABLE_* twin) are NOT handled
// here; they ride the per-session `--settings` payload via
// harness.ContextFeatureSettings.
func ApplyContextFeaturesEnv(h *harness.Harness, features map[string]string, env map[string]string) {
	if env == nil || len(features) == 0 || !h.SupportsContextFeatures() {
		return
	}
	for key, value := range harness.ContextFeatureEnv(features) {
		env[key] = value
	}
}
