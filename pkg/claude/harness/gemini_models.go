package harness

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// geminiKnownModels is a SUGGESTION list, not an allow-list. It mirrors the
// aliases and concrete ids in packages/core/src/config/models.ts at
// GeminiPinnedVersion. The aliases lead because they never go stale: Gemini
// resolves `auto`/`pro`/`flash`/`flash-lite` to whatever concrete model the
// account has access to (preview access, release channel, …).
var geminiKnownModels = []string{
	"auto",
	"pro",
	"flash",
	"flash-lite",
	"gemini-3.1-pro-preview",
	"gemini-3-pro-preview",
	"gemini-3.8-flash",
	"gemini-3.5-flash",
	"gemini-3-flash-preview",
	"gemini-3-flash",
	"gemini-3.5-flash-lite",
	"gemini-3.1-flash-lite",
	"gemini-2.5-pro",
	"gemma-4-31b-it",
}

// geminiMaxModelLen bounds a model token so an accidental paste is rejected
// as the mistake it is rather than forwarded to the CLI.
const geminiMaxModelLen = 128

// geminiModels is the ModelCatalog for Gemini CLI.
//
// Validation is permissive about WHICH Gemini model is named — the CLI owns
// that list, resolves aliases per account, and reports an unknown model
// itself — but it does reject the two things that are certainly mistakes: a
// value that is not a single bounded token, and another vendor's model.
// Gemini CLI talks only to Google's Gemini API / Vertex AI, so a `claude-*`
// or `gpt-*` slug can only mean the wrong --harness was picked, and failing
// before the pane starts is far kinder than a pane that errors on its first
// turn.
type geminiModels struct{}

// geminiForeignModelPrefixes are model namespaces Gemini CLI cannot serve.
var geminiForeignModelPrefixes = []string{"claude-", "gpt-", "o1", "o3", "o4", "codex-"}

// geminiClaudeAliases are Claude Code's model aliases. They are rejected for
// the same reason as the foreign prefixes, and named in the error so the fix
// is obvious.
var geminiClaudeAliases = []string{"fable", "opus", "sonnet", "haiku", "opusplan"}

func (geminiModels) ValidateModel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > geminiMaxModelLen {
		return "", fmt.Errorf("invalid model: must be at most %d characters, got %d",
			geminiMaxModelLen, len(s))
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("invalid model %q: a Gemini model id is a single token with no "+
				"whitespace (e.g. auto, pro, flash, gemini-3.1-pro-preview)", s)
		}
	}
	if strings.HasPrefix(s, "-") {
		return "", fmt.Errorf("invalid model %q: a model id cannot start with '-'", s)
	}
	lower := strings.ToLower(s)
	if slices.Contains(geminiClaudeAliases, strings.TrimSuffix(lower, "[1m]")) {
		return "", fmt.Errorf("model %q is a Claude Code alias; Gemini CLI serves Gemini models only "+
			"(e.g. auto, pro, flash, gemini-3.1-pro-preview) — did you mean --harness claude?", s)
	}
	for _, prefix := range geminiForeignModelPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return "", fmt.Errorf("model %q is not a Gemini model; Gemini CLI serves Gemini models only "+
				"(e.g. auto, pro, flash, gemini-3.1-pro-preview)", s)
		}
	}
	return s, nil
}

// ValidateEffort accepts only the empty value. Gemini CLI has no reasoning
// effort flag: thinking budgets/levels live in per-model generation config
// (settings.json `modelConfigs`), not on the command line, so there is nothing
// a launch could honestly forward. Rejecting beats silently dropping a value
// the caller believed took effect.
func (geminiModels) ValidateEffort(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	return "", fmt.Errorf("invalid effort %q: Gemini CLI has no reasoning-effort launch option "+
		"(configure thinking per model in Gemini's settings.json instead)", s)
}

func (geminiModels) Models() []string       { return slices.Clone(geminiKnownModels) }
func (geminiModels) EffortLevels() []string { return nil }
