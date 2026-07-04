package harness

import (
	"fmt"
	"strings"
)

// AskTimeoutCatalog is the optional capability for a harness that supports a
// launch-time AskUserQuestion idle-timeout override — Claude Code's
// `askUserQuestionTimeout` settings.json key, delivered per-session via
// `--settings` (there is no dedicated launch flag; see claude_ask_timeout.go).
// A harness with no such concept — Codex, which has no AskUserQuestion dialog —
// leaves Harness.AskTimeout nil, so SupportsAskTimeout() is false and passing a
// value is an error the caller surfaces.
//
// The contract mirrors SandboxCatalog exactly so the dashboard, CLI and profile
// editor drive their selector off it uniformly: name the default value, and
// list / validate / describe the selectable ones.
type AskTimeoutCatalog interface {
	// DefaultMode is the value a tclaude-spawned agent runs under when the
	// caller didn't choose one. For Claude Code it is `inherit` (no override),
	// which normalizes to "" — an un-chosen spawn keeps the operator's own
	// settings.json value, never silently changed. There is deliberately no
	// "agentic default" here: enabling auto-continue is an explicit per-agent /
	// per-profile / config opt-in.
	DefaultMode() string
	// ValidateMode normalizes and validates a requested value. The empty string
	// is returned unchanged (omit the override); the `inherit` sentinel also
	// normalizes to ""; any other value is either a recognized one (returned
	// trimmed) or an error naming the valid set.
	ValidateMode(mode string) (string, error)
	// Modes lists the selectable values for spawn UIs, in a stable order
	// (inherit first as the recommended default). The dashboard drives its
	// <select> off this so the harness owns its own value set.
	Modes() []string
	// ModeHelp returns a one-line human description of a value for spawn UIs, or
	// "" for an unrecognized one. The copy lives beside the values it describes
	// so the dashboard renders it verbatim and it can't drift from Modes().
	ModeHelp(mode string) string
}

// ResolveAskTimeoutMode is the entry point every spawn boundary (daemon
// spawn/resume/clone/reincarnate, `tclaude agent spawn`, the profile builder,
// direct `session new`) uses to turn a requested AskUserQuestion timeout into
// the value to thread into SpawnSpec.AskUserQuestionTimeout.
//
// Unlike ResolveSandboxMode there is no *secure* default to impose — a blank
// request resolves to the harness's DefaultMode (Claude Code: inherit), which
// ValidateMode normalizes back to "" (omit). So an un-chosen spawn adds no
// `--settings` override and keeps the operator's settings.json value: enabling
// auto-continue is always an explicit opt-in. A value requested for a harness
// with no AskUserQuestion dialog (Codex) is an error, so a mistaken carry-over
// surfaces instead of being silently dropped. requested is trimmed first.
func ResolveAskTimeoutMode(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	if !h.SupportsAskTimeout() {
		return "", fmt.Errorf("harness %q has no AskUserQuestion idle-timeout override "+
			"(askUserQuestionTimeout is a Claude Code setting; not available for this harness)", h.Name)
	}
	return h.AskTimeout.ValidateMode(requested)
}
