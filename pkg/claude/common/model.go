package common

import (
	"fmt"
	"slices"
	"strings"
)

// ValidModels are the model aliases Claude Code's `--model` flag
// accepts, the `[1m]` variants selecting the 1M-token context window.
// tclaude only ever forwards one of these to `claude`; an empty
// selection means "do not pass --model at all" so claude uses its own
// default. Keeping the set in one place means every spawn surface —
// `session new`, `agent spawn`, the agentd spawn API, and the
// dashboard modal — validates against the same list, and adding a
// future model is a one-line change here.
var ValidModels = []string{
	"fable", "fable[1m]",
	"opus", "opus[1m]",
	"sonnet", "sonnet[1m]",
	"haiku",
}

// IsValidModel reports whether s is exactly one of the known model
// aliases. It does no trimming or case-folding — callers that accept
// raw user input should go through ValidateModel, which normalises
// first. The empty string is not valid here: "" means "omit the flag"
// and is handled by callers before they validate.
func IsValidModel(s string) bool {
	return slices.Contains(ValidModels, s)
}

// ValidateModel normalises and validates a user-supplied model
// value for forwarding to `claude --model`.
//
//   - An empty or whitespace-only value returns ("", nil): the caller
//     then omits --model entirely, so claude uses its own default.
//   - A non-empty value is trimmed and lower-cased (so "Opus[1M]" is
//     accepted), then checked against ValidModels.
//   - An unknown model returns a descriptive error naming the
//     accepted set.
//
// The returned string is the cleaned alias to forward. Routing every
// surface through this one function keeps "unset omits the flag" and
// "known models only" true everywhere.
func ValidateModel(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !IsValidModel(s) {
		return "", fmt.Errorf("invalid model %q: must be one of %s", s, strings.Join(ValidModels, ", "))
	}
	return s, nil
}
