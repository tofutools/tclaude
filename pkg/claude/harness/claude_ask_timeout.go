package harness

import (
	"fmt"
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

// claudeAskTimeout is Claude Code's AskTimeoutCatalog. The default is `inherit`:
// a tclaude-spawned Claude agent's AskUserQuestion behaviour is whatever the
// operator already configured in settings.json, never silently overridden. The
// explicit values map 1:1 to Claude Code's own `askUserQuestionTimeout` enum.
type claudeAskTimeout struct{}

// DefaultMode is `inherit` — the dropdown's recommended option. It normalizes to
// "" through ValidateMode, so resolving a blank request still ends at "omit",
// leaving an un-chosen Claude spawn on the operator's own settings.json value.
func (claudeAskTimeout) DefaultMode() string { return ClaudeAskTimeoutInherit }

// Modes lists the selectable values for spawn UIs: inherit (default /
// recommended), then never, then the three auto-continue intervals in ascending
// order. A fresh slice each call so a caller can't mutate the set.
func (claudeAskTimeout) Modes() []string {
	return []string{
		ClaudeAskTimeoutInherit,
		ClaudeAskTimeoutNever,
		ClaudeAskTimeout60s,
		ClaudeAskTimeout5m,
		ClaudeAskTimeout10m,
	}
}

// ValidateMode normalizes and validates a requested value. Both "" and the
// `inherit` sentinel return "" — inherit is a recognized value that means "add
// no --settings override", so it collapses to the same omit the empty string
// already means. never / 60s / 5m / 10m return themselves; anything else is an
// error naming the valid set.
func (claudeAskTimeout) ValidateMode(mode string) (string, error) {
	m := strings.TrimSpace(mode)
	switch m {
	case "", ClaudeAskTimeoutInherit:
		return "", nil
	case ClaudeAskTimeoutNever, ClaudeAskTimeout60s, ClaudeAskTimeout5m, ClaudeAskTimeout10m:
		return m, nil
	default:
		return "", fmt.Errorf("invalid claude askUserQuestionTimeout %q (want %s|%s|%s|%s|%s)",
			mode, ClaudeAskTimeoutInherit, ClaudeAskTimeoutNever,
			ClaudeAskTimeout60s, ClaudeAskTimeout5m, ClaudeAskTimeout10m)
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
// (inherit / unset / unrecognized — the spawner omits it). Unlike the sandbox
// block this is a bare string value (Claude Code's `askUserQuestionTimeout` is a
// scalar enum), so it merges straight in under its top-level key.
func claudeAskTimeoutValue(mode string) string {
	m := strings.TrimSpace(mode)
	switch m {
	case ClaudeAskTimeoutNever, ClaudeAskTimeout60s, ClaudeAskTimeout5m, ClaudeAskTimeout10m:
		return m
	default:
		return ""
	}
}
