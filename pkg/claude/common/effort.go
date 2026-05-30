package common

import (
	"fmt"
	"slices"
	"strings"
)

// ValidEffortLevels are the reasoning-effort levels Claude Code's
// `--effort` flag accepts, in ascending order. tclaude only ever
// forwards one of these to `claude`; an empty selection means "do not
// pass --effort at all" so claude uses its own default. Keeping the
// set in one place means every spawn surface — `session new`,
// `agent spawn`, the agentd spawn API, and the dashboard modal —
// validates against the same list, and adding a future level is a
// one-line change here.
var ValidEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// IsValidEffort reports whether s is exactly one of the known effort
// levels. It does no trimming or case-folding — callers that accept
// raw user input should go through ValidateEffort, which normalises
// first. The empty string is not valid here: "" means "omit the flag"
// and is handled by callers before they validate.
func IsValidEffort(s string) bool {
	return slices.Contains(ValidEffortLevels, s)
}

// ValidateEffort normalises and validates a user-supplied effort
// value for forwarding to `claude --effort`.
//
//   - An empty or whitespace-only value returns ("", nil): the caller
//     then omits --effort entirely, so claude uses its own default.
//   - A non-empty value is trimmed and lower-cased (so "High" is
//     accepted), then checked against ValidEffortLevels.
//   - An unknown level returns a descriptive error naming the
//     accepted set.
//
// The returned string is the cleaned level to forward. Routing every
// surface through this one function keeps "unset omits the flag" and
// "known levels only" true everywhere.
func ValidateEffort(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !IsValidEffort(s) {
		return "", fmt.Errorf("invalid effort %q: must be one of %s", s, strings.Join(ValidEffortLevels, ", "))
	}
	return s, nil
}
