package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// AutoCompactWindowEnvVar names the native Claude setting. An empty choice emits no override.
const AutoCompactWindowEnvVar = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

const (
	MinAutoCompactWindow = 10_000
	MaxAutoCompactWindow = 10_000_000
)

// ParseAutoCompactWindow retains v1 token syntax and bounds with exact decimal arithmetic.
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

func ResolveAutoCompactWindow(harness string, requested string) (string, error) {
	window, err := ParseAutoCompactWindow(requested)
	if err != nil || window == "" {
		return "", err
	}
	if harness != "claude" {
		return "", fmt.Errorf("harness %q has no auto-compaction window setting "+
			"(%s is a Claude Code variable; not available for this harness)",
			harness, AutoCompactWindowEnvVar)
	}
	return window, nil
}

// EffectiveContextWindow uses the smaller known model capacity and configured compaction window.
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

func AutoCompactWindowTokens(window string) int64 {
	tokens, err := strconv.ParseInt(strings.TrimSpace(window), 10, 64)
	if err != nil || tokens <= 0 {
		return 0
	}
	return tokens
}

// RebaseContextPercentage scales the native percentage without reinterpreting native token accounting.
func RebaseContextPercentage(pct float64, modelWindow, effectiveWindow int64) float64 {
	if pct <= 0 || modelWindow <= 0 || effectiveWindow <= 0 || effectiveWindow >= modelWindow {
		return pct
	}
	rebased := pct * float64(modelWindow) / float64(effectiveWindow)
	return min(rebased, 100)
}

func FormatAutoCompactWindow(window string) string {
	tokens, err := strconv.ParseInt(strings.TrimSpace(window), 10, 64)
	if err != nil {
		return ""
	}
	return FormatContextWindowTokens(tokens)
}

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

// AutoCompactWindow stores canonical tokens; JSON authoring accepts v1 human spellings.
type AutoCompactWindow string

func (w *AutoCompactWindow) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	normalized, err := ParseAutoCompactWindow(value)
	if err != nil {
		return err
	}
	*w = AutoCompactWindow(normalized)
	return nil
}
func (w AutoCompactWindow) Validate(harness string) error {
	normalized, err := ResolveAutoCompactWindow(harness, string(w))
	if err != nil {
		return err
	}
	if normalized != string(w) {
		return fmt.Errorf("auto-compaction window requires canonical token count")
	}
	return nil
}
