package harness

import (
	"fmt"
	"strconv"
	"strings"
)

// AutoCompactWindowEnvVar is Claude Code's auto-compaction context capacity, in
// tokens.
//
// Claude Code's env-var reference (https://code.claude.com/docs/en/settings)
// documents it as: "Set the context capacity in tokens used for auto-compaction
// calculations. Defaults to the model's context window, 200K for standard
// models or 1M for extended context models, except on Sonnet 5, which has its
// own default threshold. Use a lower value like 500000 on a 1M model to treat
// the window as 500K for compaction purposes. The value is capped at the model's
// actual context window. CLAUDE_AUTOCOMPACT_PCT_OVERRIDE is applied as a
// percentage of this value. Setting this variable decouples the compaction
// threshold from the status line's used_percentage, which always uses the
// model's full context window."
//
// WHY TCLAUDE STEERS THIS. A long-lived tclaude agent on a 1M-context model
// will happily run to eight or nine hundred thousand tokens before Claude Code
// considers compacting, and answer quality degrades well before that. Pinning
// the window lower — 450K, say — makes auto-compaction fire while the agent is
// still sharp, without the operator having to babysit `/compact`. It is the
// automatic sibling of the manual lifecycle levers in `tclaude agent compact` /
// `reincarnate`.
//
// TWO CONSEQUENCES WORTH KNOWING. The value is CAPPED at the model's real
// context window, so setting 450000 on a 200K model changes nothing — it is a
// ceiling, never a floor. And once set, the compaction threshold no longer
// tracks the status line: tclaude's context percentage keeps reporting against
// the model's full window, so an agent pinned to 450K of a 1M window compacts
// at roughly 45% used rather than at the ~90% the bar would suggest.
//
// Unlike AutoMemoryEnvVar there is no "explicitly write the off value" trick
// available: the variable has no documented sentinel meaning "use the model
// default". Unset therefore means UNSET — tclaude omits the variable entirely
// and leaves the operator's own environment in charge.
const AutoCompactWindowEnvVar = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

// Bounds on a requested auto-compact window. These are typo guards, not policy:
// Claude Code caps the value at the model's real context window on its own, so
// the upper bound only has to reject an obviously slipped digit, and the lower
// bound only has to reject a window so small the agent would compact itself into
// a loop before finishing a single tool call.
const (
	MinAutoCompactWindow = 10_000
	MaxAutoCompactWindow = 10_000_000
)

// SupportsAutoCompactWindow reports whether the harness has an auto-compaction
// window tclaude can pin. This is Claude Code's knob; Codex and OpenCode manage
// their own compaction with no equivalent env var, so callers must not emit it
// for them — and must hide the affordance.
//
// Gated on the harness NAME rather than a capability func for the same reason
// SupportsAutoMemory is: it is a plain environment variable, not a lifecycle
// command with a per-harness implementation to probe.
func (h *Harness) SupportsAutoCompactWindow() bool {
	return h != nil && h.Name == DefaultName
}

// CanAutoCompactWindow is the UI-side predicate a spawn/profile control gates on
// (mirrors CanAutoMemory).
func (h *Harness) CanAutoCompactWindow() bool {
	return h.SupportsAutoCompactWindow()
}

// ParseAutoCompactWindow normalizes the human spelling of a token window to its
// canonical decimal form — the string every layer below stores and compares.
//
//	""            → ""        (unset)
//	"450000"      → "450000"
//	"450_000"     → "450000"  (underscores are digit separators)
//	"450k", "450K"→ "450000"
//	"0.5M", "1m"  → "500000", "1000000"
//
// A suffixed value may carry a fraction as long as it lands on a whole token
// ("0.5M" is fine, "1.0005k" is not) — the arithmetic is done on the digit
// string rather than in float64, so a value near the upper bound cannot drift.
// Out-of-range values are rejected here rather than clamped: an operator who
// typed 4500000 meaning 450000 wants to hear about it, not to silently get the
// model default back after Claude Code caps it.
func ParseAutoCompactWindow(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, " ", "")

	// Trailing unit suffix shifts the decimal point by a fixed number of places.
	shift := 0
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		shift, s = 3, s[:len(s)-1]
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "M"):
		shift, s = 6, s[:len(s)-1]
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if hasFrac && fracPart == "" {
		// "450." — a trailing point with no fraction is a typo, not a value.
		return "", autoCompactWindowSyntaxError(raw)
	}
	if intPart == "" && fracPart == "" {
		return "", autoCompactWindowSyntaxError(raw)
	}
	if !isDigits(intPart) || !isDigits(fracPart) {
		return "", autoCompactWindowSyntaxError(raw)
	}
	if len(fracPart) > shift {
		return "", fmt.Errorf("invalid auto-compact window %q: %s tokens is not a whole number of tokens",
			raw, strings.TrimSpace(raw))
	}

	// Shift the decimal point right by `shift` places: the fraction digits are
	// consumed first, then the remainder is padded with zeros.
	digits := strings.TrimLeft(intPart+fracPart+strings.Repeat("0", shift-len(fracPart)), "0")
	if digits == "" {
		digits = "0"
	}
	if len(digits) > 15 {
		return "", autoCompactWindowRangeError(raw)
	}
	tokens, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return "", autoCompactWindowSyntaxError(raw)
	}
	if tokens < MinAutoCompactWindow || tokens > MaxAutoCompactWindow {
		return "", autoCompactWindowRangeError(raw)
	}
	return strconv.FormatInt(tokens, 10), nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func autoCompactWindowSyntaxError(raw string) error {
	return fmt.Errorf("invalid auto-compact window %q (want a token count such as 450000, 450k or 0.5M)", raw)
}

func autoCompactWindowRangeError(raw string) error {
	return fmt.Errorf("auto-compact window %q out of range (want %d–%d tokens)",
		raw, MinAutoCompactWindow, MaxAutoCompactWindow)
}

// ResolveAutoCompactWindow is the entry point every spawn boundary (daemon
// spawn/resume/clone/reincarnate, `tclaude agent spawn`, the profile builder,
// direct `session new`) routes a requested window through.
//
// A blank request resolves to "" — no variable is emitted and Claude Code's own
// per-model default decides, so an un-chosen spawn never silently changes
// compaction behaviour. Requesting a window for a harness that has no such knob
// is an error rather than a silent drop, so a value carried over from a Claude
// profile onto a Codex spawn surfaces at the boundary instead of vanishing at
// runtime — the same contract ResolveAutoMemory and ResolveContextFeatures
// apply.
func ResolveAutoCompactWindow(h *Harness, requested string) (string, error) {
	window, err := ParseAutoCompactWindow(requested)
	if err != nil || window == "" {
		return "", err
	}
	if !h.CanAutoCompactWindow() {
		return "", fmt.Errorf("harness %q has no auto-compaction window setting "+
			"(%s is a Claude Code variable; not available for this harness)",
			harnessName(h), AutoCompactWindowEnvVar)
	}
	return window, nil
}

// EffectiveContextWindow is the window every context percentage, meter and bar
// in tclaude is measured against: the SMALLER of the model's real context
// window and any pinned auto-compaction window.
//
// Both arguments may be 0 for "unknown": a pin of 0 means nothing was pinned, so
// the model's window stands; a model window of 0 means the status line has not
// reported one yet, so the pin stands. If both are 0 the answer is 0 and the
// caller has nothing to measure against.
//
// The MIN is what makes the number honest in both directions. Claude Code caps
// the pin at the model's real window, so pinning 450K on a 200K model changes
// nothing and a meter drawn against 450K would understate how full the agent
// actually is. And when the pin is the smaller one, it — not the model window —
// is where compaction actually fires, which is the event the operator is
// watching for.
func EffectiveContextWindow(modelWindow, pinnedWindow int64) int64 {
	switch {
	case modelWindow <= 0:
		return max(pinnedWindow, 0)
	case pinnedWindow <= 0:
		return modelWindow
	default:
		return min(modelWindow, pinnedWindow)
	}
}

// AutoCompactWindowTokens parses a stored canonical window into a token count,
// returning 0 for "" / anything unparseable. The convenience twin of
// EffectiveContextWindow for the many callers holding the string form.
func AutoCompactWindowTokens(window string) int64 {
	tokens, err := strconv.ParseInt(strings.TrimSpace(window), 10, 64)
	if err != nil || tokens <= 0 {
		return 0
	}
	return tokens
}

// RebaseContextPercentage converts a percentage measured against modelWindow
// into one measured against the effective window, and clamps it to 0–100.
//
// It rescales the harness's OWN percentage rather than recomputing from token
// counts on purpose: Claude Code decides what counts toward its context figure,
// and re-deriving that from total_input + total_output would quietly disagree
// with the number the harness itself reports. Changing only the denominator
// keeps tclaude's meter a re-basing of Claude Code's answer, not a second
// opinion about it.
//
// A zero/unknown window on either side leaves the percentage untouched, as does
// a pin at or above the model's window (where Claude Code's cap makes the pin a
// no-op). The clamp matters because an agent can sit briefly above a pinned
// window before compaction lands, and a 130% bar would render as garbage.
func RebaseContextPercentage(pct float64, modelWindow, effectiveWindow int64) float64 {
	if pct <= 0 || modelWindow <= 0 || effectiveWindow <= 0 || effectiveWindow >= modelWindow {
		return pct
	}
	rebased := pct * float64(modelWindow) / float64(effectiveWindow)
	return min(rebased, 100)
}

// FormatAutoCompactWindow renders a canonical window for CLI output, spawn
// notes and log lines ("450000" → "450k"). Values that are not a round multiple
// of 1000 keep their digits, so nothing is ever rounded away in a message an
// operator might act on. "" (unset) renders as "".
func FormatAutoCompactWindow(window string) string {
	tokens, err := strconv.ParseInt(strings.TrimSpace(window), 10, 64)
	if err != nil {
		return ""
	}
	return FormatContextWindowTokens(tokens)
}

// FormatContextWindowTokens is FormatAutoCompactWindow for a caller already
// holding a token count rather than the stored string form — notably the status
// line, which labels its bar with the EFFECTIVE window: a min of two integers,
// so never a stored string.
//
// 0 (and any negative) renders as "", which callers treat as "omit the marker".
// That is the COMMON case, not an error: most agents run with no pinned window
// at all, and a status line that has not yet seen a context_window_size from the
// harness has no window to name either. Neither deserves a diagnostic.
func FormatContextWindowTokens(tokens int64) string {
	switch {
	case tokens <= 0:
		return ""
	case tokens%1_000_000 == 0:
		return strconv.FormatInt(tokens/1_000_000, 10) + "M"
	case tokens%1_000 == 0:
		return strconv.FormatInt(tokens/1_000, 10) + "k"
	default:
		return strconv.FormatInt(tokens, 10)
	}
}
