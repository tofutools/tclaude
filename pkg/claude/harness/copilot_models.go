package harness

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// copilotKnownModels is a SUGGESTION list, not an allow-list. It mirrors the
// documented "Supported models" table plus `auto` (Copilot picks the best
// available model), and gives every ModelCatalog-driven surface — the spawn
// dialog, profiles, roles, template-local launch profiles — the same dropdown.
//
// `auto` leads because it is the choice that never goes stale.
var copilotKnownModels = []string{
	"auto",
	"claude-sonnet-4.6",
	"claude-haiku-4.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.1-pro-preview",
	"gemini-3.5-flash",
	"gemini-3.6-flash",
	"mai-code-1-flash",
}

// copilotMaxModelLen bounds a model token. Copilot's own ids are far shorter;
// this only exists so an accidental paste (a prompt, a file, a whole command
// line) is rejected as the mistake it is rather than forwarded to the CLI.
const copilotMaxModelLen = 128

// copilotModels is the ModelCatalog for GitHub Copilot CLI.
//
// Validation is deliberately PERMISSIVE. Copilot brokers models from several
// vendors at once — the documented set spans `claude-*`, `gpt-*`, `gemini-*`
// and `mai-*` — and exposes no machine-readable catalog, so the list above
// would go stale within a release. Curating it as an allow-list would also
// mean rejecting `claude-sonnet-4.6` (a Claude Code slug that IS a valid
// Copilot model), so the cross-harness rejection Codex performs is exactly
// wrong here: for Copilot there is no model namespace that identifies "the
// wrong harness".
//
// What remains is a safety gate rather than a knowledge gate: one non-empty
// token, of bounded length, carrying no whitespace or control characters.
// Copilot owns the authoritative per-release validation and reports an unknown
// model itself, and the spawner shell-quotes the value, so this deliberately
// does NOT curate an allowed character set — a custom/BYOK id tclaude has
// never seen must still be forwardable.
type copilotModels struct{}

// copilotEffortLevels is the exact `--effort` vocabulary advertised by the
// pinned Copilot CLI release (1.0.77). It intentionally lives beside the
// Copilot catalog rather than widening common.ValidEffortLevels: Claude Code
// and Codex have their own, separately pinned vocabularies.
var copilotEffortLevels = []string{
	"none",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
}

// ValidateModel normalizes a model token for `copilot --model=<model>`.
// Empty stays empty → the spawner omits the flag and Copilot uses its own
// configured default. A non-empty value is trimmed and then gated.
//
// Case is PRESERVED. Copilot's own ids are lower-case, but nothing documents
// model matching as case-insensitive, and custom/BYOK deployment ids may be
// mixed-case — folding them would silently corrupt a valid id, which is a
// worse failure than passing an unusual one through.
func (copilotModels) ValidateModel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > copilotMaxModelLen {
		return "", fmt.Errorf("invalid model: must be at most %d characters, got %d",
			copilotMaxModelLen, len(s))
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("invalid model %q: a Copilot model id is a single token with no "+
				"whitespace (e.g. auto, claude-sonnet-4.6, gpt-5.4)", s)
		}
	}
	return s, nil
}

// ValidateEffort accepts Copilot's documented `--effort=LEVEL` values and
// forwards the normalized token verbatim. Empty remains empty so the spawner
// omits the flag and Copilot performs its own automatic selection.
func (copilotModels) ValidateEffort(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !slices.Contains(copilotEffortLevels, s) {
		return "", fmt.Errorf("invalid effort %q: must be one of %s", s, strings.Join(copilotEffortLevels, ", "))
	}
	return s, nil
}

// Models returns a copy of the curated suggestions. ValidateModel remains the
// authority and accepts ids outside this list.
func (copilotModels) Models() []string       { return slices.Clone(copilotKnownModels) }
func (copilotModels) EffortLevels() []string { return slices.Clone(copilotEffortLevels) }
