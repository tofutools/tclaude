package harness

import (
	"fmt"
	"slices"
	"strings"
)

// Claude Code AskUserQuestion idle-timeout modes. Claude Code (>= 2.1.x) no
// longer auto-continues an AskUserQuestion dialog by default — it waits for a
// human — and the auto-continue is opt-in via the `askUserQuestionTimeout`
// settings.json key, an enum of never|60s|5m|10m (validated against Claude
// Code's own option set). For a tclaude-spawned *agent*, which runs unattended
// with no human at its pane, that default means it STALLS the moment it raises a
// question; a timeout is what keeps it acting agentically (auto-continue with
// the dialog's default answer after the idle interval).
//
// Like the OS sandbox (claude_sandbox.go), Claude Code exposes no launch flag
// for this — the per-session lever is `claude --settings '<json>'`, which merges
// a settings block over the user/project files (only managed/policy settings
// outrank it). So tclaude models a small tri-plus-state and translates it to a
// `--settings` override in claudeSpawner.BuildCommand, MERGED with the sandbox
// block into one `--settings` payload (see claudeSettingsJSON):
//
//   - inherit : add no override — the agent uses the operator's own
//     settings.json value (absent the key, that is Claude Code's "never"
//     default). This is the DEFAULT, so an un-chosen spawn never silently
//     changes the operator's configured behaviour. It NORMALIZES to "" (omit)
//     — see ValidateMode.
//   - never   : force NO auto-continue for this session — the dialog waits for
//     a human — even if settings.json opts in.
//   - 60s / 5m / 10m : auto-continue the dialog with its default answer after
//     the idle interval. The agentic choice for an unattended worker.
const (
	ClaudeAskTimeoutInherit = "inherit"
	ClaudeAskTimeoutNever   = "never"
	ClaudeAskTimeout60s     = "60s"
	ClaudeAskTimeout5m      = "5m"
	ClaudeAskTimeout10m     = "10m"
)

// claudeAskTimeoutRealModes is the single source of truth for the concrete
// `askUserQuestionTimeout` values (everything except the inherit sentinel) —
// the ones that emit an actual settings.json override. Modes(), ValidateMode,
// claudeAskTimeoutValue and the error copy all derive their value set from this
// slice so the four can never drift (a fifth interval is a one-line addition
// here). A package-level slice is returned by value through a defensive copy
// where a caller could mutate it (see Modes).
var claudeAskTimeoutRealModes = []string{
	ClaudeAskTimeoutNever,
	ClaudeAskTimeout60s,
	ClaudeAskTimeout5m,
	ClaudeAskTimeout10m,
}

// isRealAskTimeout reports whether m (already trimmed) is one of the concrete
// override values — i.e. not the inherit sentinel / unset.
func isRealAskTimeout(m string) bool {
	return slices.Contains(claudeAskTimeoutRealModes, m)
}

// claudeAskTimeout is Claude Code's AskTimeoutCatalog. The default is `inherit`:
// a tclaude-spawned Claude agent's AskUserQuestion behaviour is whatever the
// operator already configured in settings.json, never silently overridden. The
// explicit values map 1:1 to Claude Code's own `askUserQuestionTimeout` enum.
type claudeAskTimeout struct{}

// DefaultMode is `inherit` — the dropdown's recommended option. `inherit` is a
// FIRST-CLASS value (ValidateMode returns it unchanged, NOT ""): it means "use
// the operator's own settings.json as-is AND don't let a profile/group default
// override that". It collapses to omit only at the final `--settings` emission
// (see claudeAskTimeoutValue), so a spawn that explicitly chose inherit keeps
// the operator's value and is not silently re-filled by an overlay.
func (claudeAskTimeout) DefaultMode() string { return ClaudeAskTimeoutInherit }

// Modes lists the selectable values for spawn UIs: inherit (default /
// recommended) first, then the concrete override values in their canonical
// order. A fresh slice each call so a caller can't mutate the set.
func (claudeAskTimeout) Modes() []string {
	return append([]string{ClaudeAskTimeoutInherit}, claudeAskTimeoutRealModes...)
}

// ValidateMode normalizes and validates a requested value, preserving the
// tri-state the overlay sites depend on:
//
//   - ""      → "" (OMITTED — a higher level, e.g. a group default profile, may
//     fill it; if nothing does, the final emission adds no override).
//   - inherit → "inherit" (ACTIVELY chosen — carried through as a first-class
//     sentinel so an overlay treats it as "already set" and does NOT overwrite
//     it; the final emission collapses it to no override).
//   - never / 60s / 5m / 10m → themselves.
//   - anything else → an error naming the valid set.
//
// Collapsing inherit to "" here (the old behaviour) is exactly what made an
// explicit inherit indistinguishable from omitted, so a group default silently
// won — the JOH bug this preserves the distinction to fix.
func (claudeAskTimeout) ValidateMode(mode string) (string, error) {
	m := strings.TrimSpace(mode)
	switch {
	case m == "":
		return "", nil
	case m == ClaudeAskTimeoutInherit:
		return ClaudeAskTimeoutInherit, nil
	case isRealAskTimeout(m):
		return m, nil
	default:
		return "", fmt.Errorf("invalid claude askUserQuestionTimeout %q (want %s|%s)",
			mode, ClaudeAskTimeoutInherit, strings.Join(claudeAskTimeoutRealModes, "|"))
	}
}

// claudeAskTimeoutModeHelp is the one-line description the spawn UI shows per
// value. Keyed by value; inherit carries no ⚠ (it's the safe default).
var claudeAskTimeoutModeHelp = map[string]string{
	ClaudeAskTimeoutInherit: "Recommended. No per-session override — the agent uses your Claude Code settings.json `askUserQuestionTimeout` as-is (absent the key, Claude Code's default is 'never': it waits for a human).",
	ClaudeAskTimeoutNever:   "The AskUserQuestion dialog waits indefinitely for a human answer — never auto-continues — even if your settings.json opts in. Use for an agent whose questions must always reach you.",
	ClaudeAskTimeout60s:     "Auto-continue the dialog with its default answer after 60s idle. Keeps an unattended agent moving instead of stalling on a question.",
	ClaudeAskTimeout5m:      "Auto-continue the dialog with its default answer after 5m idle.",
	ClaudeAskTimeout10m:     "Auto-continue the dialog with its default answer after 10m idle.",
}

// ModeHelp returns a one-line description of a value for spawn UIs, or "" for an
// unrecognized value. The inherit help is keyed under its token even though
// ValidateMode collapses it to "" — the dashboard renders help off the raw
// Modes() tokens, not the validated value.
func (claudeAskTimeout) ModeHelp(mode string) string {
	return claudeAskTimeoutModeHelp[strings.TrimSpace(mode)]
}

// claudeAskTimeoutValue returns the settings.json value the `--settings` payload
// should carry for a validated mode, or "" when no override should be emitted
// (inherit / unset / unrecognized — the spawner omits it). This is where the
// first-class `inherit` sentinel collapses to "omit", the LAST layer that sees
// it. Unlike the sandbox block this is a bare string value (Claude Code's
// `askUserQuestionTimeout` is a scalar enum), so it merges straight in under its
// top-level key.
func claudeAskTimeoutValue(mode string) string {
	if m := strings.TrimSpace(mode); isRealAskTimeout(m) {
		return m
	}
	return ""
}
