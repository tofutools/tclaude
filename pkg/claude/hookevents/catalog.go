// Package hookevents owns the harness-native hook vocabulary that standing
// orders may select. It intentionally has no dependency on the harness,
// database, or session packages so all three layers can share one catalog
// without an import cycle.
package hookevents

import (
	"sort"
	"strings"
)

const (
	HarnessClaude   = "claude"
	HarnessCodex    = "codex"
	HarnessOpenCode = "opencode"
)

// Selector is one exact harness-native hook branch. A standing order with
// several selectors fires when ANY branch matches.
type Selector struct {
	Harness string `json:"harness"`
	Event   string `json:"event"`
}

// Definition describes one hook an operator may select.
//
// Baseline means tclaude already registers this hook for ordinary status and
// lifecycle tracking. Non-baseline Claude/Codex hooks are registered only
// while an enabled standing order explicitly selects them. OpenCode events
// arrive on its existing SSE stream and therefore need no declaration.
//
// SameContinuation records the conservative, tested set of events whose
// response channel can carry model-visible additional context. Every event can
// still use the durable next-turn message transport.
type Definition struct {
	Selector
	Baseline         bool `json:"baseline"`
	SameContinuation bool `json:"same_continuation"`
}

var definitions = []Definition{
	// Claude Code. Event names follow the public hooks reference.
	def(HarnessClaude, "SessionStart", true, true),
	def(HarnessClaude, "Setup", false, true),
	def(HarnessClaude, "UserPromptSubmit", true, true),
	def(HarnessClaude, "UserPromptExpansion", false, true),
	def(HarnessClaude, "PreToolUse", true, true),
	def(HarnessClaude, "PermissionRequest", true, false),
	def(HarnessClaude, "PermissionDenied", false, false),
	def(HarnessClaude, "PostToolUse", true, true),
	def(HarnessClaude, "PostToolUseFailure", true, true),
	def(HarnessClaude, "PostToolBatch", false, true),
	def(HarnessClaude, "Notification", true, false),
	def(HarnessClaude, "MessageDisplay", false, false),
	def(HarnessClaude, "SubagentStart", true, true),
	def(HarnessClaude, "SubagentStop", true, false),
	def(HarnessClaude, "TaskCreated", false, false),
	def(HarnessClaude, "TaskCompleted", false, false),
	def(HarnessClaude, "Stop", true, false),
	def(HarnessClaude, "StopFailure", true, false),
	def(HarnessClaude, "TeammateIdle", false, false),
	def(HarnessClaude, "InstructionsLoaded", false, false),
	def(HarnessClaude, "ConfigChange", false, false),
	def(HarnessClaude, "CwdChanged", false, false),
	def(HarnessClaude, "FileChanged", false, false),
	def(HarnessClaude, "WorktreeCreate", false, false),
	def(HarnessClaude, "WorktreeRemove", false, false),
	def(HarnessClaude, "PreCompact", true, false),
	def(HarnessClaude, "PostCompact", true, false),
	def(HarnessClaude, "Elicitation", false, false),
	def(HarnessClaude, "ElicitationResult", false, false),
	def(HarnessClaude, "SessionEnd", true, false),

	// Codex CLI. Event names follow the public config reference. PostCompact
	// is retained because supported Codex versions already emit it and
	// tclaude has registered it since hook support was introduced.
	def(HarnessCodex, "SessionStart", true, true),
	def(HarnessCodex, "UserPromptSubmit", true, true),
	def(HarnessCodex, "Stop", true, false),
	def(HarnessCodex, "PreToolUse", true, true),
	def(HarnessCodex, "PostToolUse", true, true),
	def(HarnessCodex, "PermissionRequest", true, false),
	def(HarnessCodex, "PreCompact", true, false),
	def(HarnessCodex, "PostCompact", true, false),
	def(HarnessCodex, "SubagentStart", true, false),
	def(HarnessCodex, "SubagentStop", true, false),
	def(HarnessCodex, "SessionEnd", false, false),

	// OpenCode. These are native event-stream/plugin names. The question.* and
	// permission.v2.* spellings are emitted by released servers even though
	// some documentation versions list only the unversioned forms.
	def(HarnessOpenCode, "command.executed", true, false),
	def(HarnessOpenCode, "file.edited", true, false),
	def(HarnessOpenCode, "file.watcher.updated", true, false),
	def(HarnessOpenCode, "installation.updated", true, false),
	def(HarnessOpenCode, "lsp.client.diagnostics", true, false),
	def(HarnessOpenCode, "lsp.updated", true, false),
	def(HarnessOpenCode, "message.part.removed", true, false),
	def(HarnessOpenCode, "message.part.updated", true, false),
	def(HarnessOpenCode, "message.removed", true, false),
	def(HarnessOpenCode, "message.updated", true, false),
	def(HarnessOpenCode, "permission.asked", true, false),
	def(HarnessOpenCode, "permission.replied", true, false),
	def(HarnessOpenCode, "permission.v2.asked", true, false),
	def(HarnessOpenCode, "permission.v2.replied", true, false),
	def(HarnessOpenCode, "question.asked", true, false),
	def(HarnessOpenCode, "question.replied", true, false),
	def(HarnessOpenCode, "question.rejected", true, false),
	def(HarnessOpenCode, "question.v2.asked", true, false),
	def(HarnessOpenCode, "question.v2.replied", true, false),
	def(HarnessOpenCode, "question.v2.rejected", true, false),
	def(HarnessOpenCode, "server.connected", true, false),
	def(HarnessOpenCode, "session.created", true, false),
	def(HarnessOpenCode, "session.compacted", true, false),
	def(HarnessOpenCode, "session.deleted", true, false),
	def(HarnessOpenCode, "session.diff", true, false),
	def(HarnessOpenCode, "session.error", true, false),
	def(HarnessOpenCode, "session.idle", true, false),
	def(HarnessOpenCode, "session.status", true, false),
	def(HarnessOpenCode, "session.updated", true, false),
	def(HarnessOpenCode, "todo.updated", true, false),
	def(HarnessOpenCode, "shell.env", true, false),
	def(HarnessOpenCode, "tool.execute.after", true, false),
	def(HarnessOpenCode, "tool.execute.before", true, false),
	def(HarnessOpenCode, "tui.prompt.append", true, false),
	def(HarnessOpenCode, "tui.command.execute", true, false),
	def(HarnessOpenCode, "tui.toast.show", true, false),
}

func def(harness, event string, baseline, sameContinuation bool) Definition {
	return Definition{
		Selector:         Selector{Harness: harness, Event: event},
		Baseline:         baseline,
		SameContinuation: sameContinuation,
	}
}

// All returns a stable copy of the catalog for API/UI projection.
func All() []Definition {
	out := append([]Definition(nil), definitions...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Harness != out[j].Harness {
			return out[i].Harness < out[j].Harness
		}
		return out[i].Event < out[j].Event
	})
	return out
}

// NormalizeSelectors trims, validates, de-duplicates, and sorts selectors.
// Unknown selectors are retained so Valid can produce a useful rejection.
func NormalizeSelectors(in []Selector) []Selector {
	seen := map[Selector]struct{}{}
	out := make([]Selector, 0, len(in))
	for _, selector := range in {
		selector.Harness = strings.ToLower(strings.TrimSpace(selector.Harness))
		selector.Event = strings.TrimSpace(selector.Event)
		if selector.Harness == "" && selector.Event == "" {
			continue
		}
		if _, duplicate := seen[selector]; duplicate {
			continue
		}
		seen[selector] = struct{}{}
		out = append(out, selector)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Harness != out[j].Harness {
			return out[i].Harness < out[j].Harness
		}
		return out[i].Event < out[j].Event
	})
	return out
}

func lookup(selector Selector) (Definition, bool) {
	for _, definition := range definitions {
		if definition.Selector == selector {
			return definition, true
		}
	}
	return Definition{}, false
}

// Valid reports whether selector is an exact catalog entry.
func Valid(selector Selector) bool {
	_, ok := lookup(selector)
	return ok
}

// SupportsSameContinuation reports the conservative response capability for
// one exact native event.
func SupportsSameContinuation(selector Selector) bool {
	definition, ok := lookup(selector)
	return ok && definition.SameContinuation
}

// BaselineEvents returns the native events tclaude already captures for a
// harness independent of standing orders.
func BaselineEvents(harness string) []string {
	harness = strings.ToLower(strings.TrimSpace(harness))
	var out []string
	for _, definition := range definitions {
		if definition.Harness == harness && definition.Baseline {
			out = append(out, definition.Event)
		}
	}
	sort.Strings(out)
	return out
}
