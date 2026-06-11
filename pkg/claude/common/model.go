package common

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ValidModels are the model aliases Claude Code's `--model` flag
// accepts, the `[1m]` variants selecting the 1M-token context window
// and "opusplan" meaning "Opus in plan mode, Sonnet otherwise".
// tclaude only ever forwards one of these to `claude`; an empty
// selection means "do not pass --model at all" so claude uses its own
// default. Keeping the set in one place means every spawn surface —
// `session new`, `agent spawn`, the agentd spawn API, and the
// dashboard modal — validates against the same list, and adding a
// future model is a one-line change here.
//
// There is no `claude models`-style command to enumerate this set
// from, so it is curated by hand against the aliases the installed
// Claude Code build resolves. Full model IDs are accepted separately
// (see fullModelIDRe) precisely so a brand-new model is usable before
// this list catches up.
var ValidModels = []string{
	"fable", "fable[1m]",
	"opus", "opus[1m]",
	"sonnet", "sonnet[1m]",
	"haiku",
	"opusplan",
}

// fullModelIDRe matches a full Claude model ID — e.g. "claude-fable-5"
// or "claude-sonnet-4-6" — with an optional [1m] context-window
// suffix, which Claude Code accepts on full IDs the same way it does
// on aliases (the user-level settings.json "model" value is commonly
// stored as "claude-fable-5[1m]"). `claude --model` takes "an alias
// ... or a model's full name", so tclaude passes full IDs through
// rather than maintaining an exhaustive ID list that would go stale
// with every model release.
var fullModelIDRe = regexp.MustCompile(`^claude-[a-z0-9][a-z0-9.-]*(\[1m\])?$`)

// IsValidModel reports whether s is one of the known model aliases or
// a full Claude model ID (see fullModelIDRe). It does no trimming or
// case-folding — callers that accept raw user input should go through
// ValidateModel, which normalises first. The empty string is not valid
// here: "" means "omit the flag" and is handled by callers before they
// validate.
func IsValidModel(s string) bool {
	return slices.Contains(ValidModels, s) || fullModelIDRe.MatchString(s)
}

// ValidateModel normalises and validates a user-supplied model
// value for forwarding to `claude --model`.
//
//   - An empty or whitespace-only value returns ("", nil): the caller
//     then omits --model entirely, so claude uses its own default.
//   - A non-empty value is trimmed and lower-cased (so "Opus[1M]" is
//     accepted), then checked against ValidModels and the full-ID
//     pattern (model IDs are lower-case, so folding is lossless).
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
		return "", fmt.Errorf("invalid model %q: must be one of %s, or a full model ID like claude-fable-5",
			s, strings.Join(ValidModels, ", "))
	}
	return s, nil
}
