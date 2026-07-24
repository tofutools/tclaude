package harness

import (
	"fmt"
	"sort"
	"strings"
)

// Per-agent startup-context trimming (TCL-597).
//
// Claude Code loads a substantial amount of always-on startup context: bundled
// skills, tool schemas for features a given agent will never touch (Artifact,
// Workflow, Cron, the advisor tool, …), and several system-prompt blocks. For a
// focused worker agent most of that is pure context-rot risk — tokens spent
// describing capabilities the agent was not spawned to use, competing for its
// attention with the task it WAS spawned for.
//
// This file is the catalog of the trims tclaude can steer per agent. Each entry
// is one Claude Code feature the operator can leave alone (the default), force
// OFF (trim it out of the agent's startup context), or force ON (keep it even
// when a lower tier trimmed it). The catalog is the single source of truth: the
// wire validation, the CLI flag help, the dashboard selector, and the launch
// injection all read it, so adding a trim is a one-entry change here.
//
// HOW A TRIM IS DELIVERED. Most entries ride a CLAUDE_CODE_DISABLE_* env var,
// which is why tclaude never has to edit the operator's settings.json — the
// same reasoning as AutoMemoryEnvVar. The few features with no env twin ride
// the per-session `--settings` payload instead (claudeSettingsJSON).
//
// A CAVEAT WORTH KEEPING. These switches were catalogued from a specific Claude
// Code build (v2.1.215) rather than from a stable documented contract, and
// Claude Code is free to rename or retire any of them. A trim that stops
// working degrades in the safe direction — the agent keeps a capability it did
// not need — so a stale entry costs context, never correctness. Re-verify the
// list when Claude Code's startup context visibly changes shape; `/context`
// inside a spawned agent is the cheap check.

// Context-feature states. The map tclaude stores holds only non-default
// entries, exactly like a permission-override map holds only real overrides.
const (
	// ContextFeatureDefault is the absent state: tclaude injects nothing and
	// Claude Code's own default (or the operator's own settings) decides.
	ContextFeatureDefault = "default"
	// ContextFeatureOn forces the feature to stay available.
	ContextFeatureOn = "on"
	// ContextFeatureOff trims the feature out of the agent's startup context.
	ContextFeatureOff = "off"
)

// ContextFeature is one steerable Claude Code startup-context feature.
type ContextFeature struct {
	// Slug is the stable wire/CLI identifier. It never changes once shipped,
	// because it is persisted in spawn profiles and group templates.
	Slug string
	// Label is the short human name the dashboard row shows.
	Label string
	// Descr is the one-line explanation of what disabling it costs and saves.
	Descr string
	// EnvVar is the CLAUDE_CODE_DISABLE_* twin, or "" for a settings-only
	// feature. Exactly one of EnvVar / SettingsKey is set.
	EnvVar string
	// SettingsKey is the settings.json `disable*` key for a feature with no env
	// twin. Delivered through the per-session `--settings` payload.
	SettingsKey string
	// Heavy marks the trims with the largest startup-context payoff, so the UI
	// can point an operator at them first.
	Heavy bool
	// Caution, when non-empty, is the reason this trim needs a deliberate
	// decision rather than a casual click. Surfaced verbatim in the UI.
	Caution string
}

// contextFeatureCatalog is the ordered catalog. Order is presentation order:
// the heavy, safe wins first, the sharp-edged ones last.
var contextFeatureCatalog = []ContextFeature{
	{
		Slug:   "bundled-skills",
		Label:  "Bundled skills",
		Descr:  "Claude Code's own shipped skills (dataviz, artifact design, pdf, …). Usually the single biggest startup-context win for a coding agent.",
		EnvVar: "CLAUDE_CODE_DISABLE_BUNDLED_SKILLS",
		Heavy:  true,
	},
	{
		Slug:   "workflows",
		Label:  "Workflows",
		Descr:  "The Workflow multi-agent orchestration tool. Its schema is one of the largest single tool definitions in the harness.",
		EnvVar: "CLAUDE_CODE_DISABLE_WORKFLOWS",
		Heavy:  true,
	},
	{
		Slug:   "artifact",
		Label:  "Artifacts",
		Descr:  "The Artifact publishing tool and its design skills. Irrelevant to an agent that only edits code.",
		EnvVar: "CLAUDE_CODE_DISABLE_ARTIFACT",
		Heavy:  true,
	},
	{
		Slug:   "explore-plan-agents",
		Label:  "Explore/Plan subagents",
		Descr:  "The built-in Explore and Plan subagent definitions, listed in every agent's tool context.",
		EnvVar: "CLAUDE_CODE_DISABLE_EXPLORE_PLAN_AGENTS",
	},
	{
		Slug:   "cron",
		Label:  "Cron tools",
		Descr:  "The CronCreate/CronList/CronDelete scheduling tools. tclaude has its own scheduler (tclaude agent cron).",
		EnvVar: "CLAUDE_CODE_DISABLE_CRON",
	},
	{
		Slug:   "background-tasks",
		Label:  "Background tasks",
		Descr:  "Claude Code's background task tools. Note this also removes background Bash, which some workflows rely on.",
		EnvVar: "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS",
	},
	{
		Slug:   "agent-view",
		Label:  "Agent view",
		Descr:  "The in-harness agent/fleet view. tclaude's dashboard already covers this.",
		EnvVar: "CLAUDE_CODE_DISABLE_AGENT_VIEW",
	},
	{
		Slug:   "advisor-tool",
		Label:  "Advisor tool",
		Descr:  "The advisor tool definition.",
		EnvVar: "CLAUDE_CODE_DISABLE_ADVISOR_TOOL",
	},
	{
		Slug:   "claude-code-skill",
		Label:  "Claude Code skill",
		Descr:  "The bundled skill that documents Claude Code's own settings, hooks and slash commands.",
		EnvVar: "CLAUDE_CODE_DISABLE_CLAUDE_CODE_SKILL",
	},
	{
		Slug:   "claude-api-skill",
		Label:  "Claude API skill",
		Descr:  "The bundled Claude API / SDK reference skill, with its always-loaded trigger instructions.",
		EnvVar: "CLAUDE_CODE_DISABLE_CLAUDE_API_SKILL",
	},
	{
		Slug:   "policy-skills",
		Label:  "Policy skills",
		Descr:  "Organization policy skills injected at startup.",
		EnvVar: "CLAUDE_CODE_DISABLE_POLICY_SKILLS",
	},
	{
		Slug:   "git-instructions",
		Label:  "Git instructions",
		Descr:  "The system-prompt block describing commit/PR conventions. Trim it when the repo's own AGENTS.md already says how to commit.",
		EnvVar: "CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS",
	},
	{
		Slug:   "org-memory",
		Label:  "Org memory",
		Descr:  "Organization-level memory files loaded into every session.",
		EnvVar: "CLAUDE_CODE_DISABLE_ORG_MEMORY",
	},
	{
		Slug:        "claude-ai-connectors",
		Label:       "claude.ai connectors",
		Descr:       "The Gmail / Drive / Calendar connector tools. Does not affect ordinary MCP servers such as Linear.",
		SettingsKey: "disableClaudeAiConnectors",
	},
	{
		Slug:    "claude-mds",
		Label:   "CLAUDE.md / AGENTS.md",
		Descr:   "The project and user memory files themselves.",
		EnvVar:  "CLAUDE_CODE_DISABLE_CLAUDE_MDS",
		Caution: "This drops the repo's own agent instructions — including this project's CLAUDE.md. An agent expected to follow project conventions should keep it.",
	},
}

// ContextFeatures returns the catalog in presentation order. The slice is a
// copy, so a caller cannot mutate the registry.
func ContextFeatures() []ContextFeature {
	return append([]ContextFeature{}, contextFeatureCatalog...)
}

// LookupContextFeature resolves a slug to its catalog entry.
func LookupContextFeature(slug string) (ContextFeature, bool) {
	for _, f := range contextFeatureCatalog {
		if f.Slug == slug {
			return f, true
		}
	}
	return ContextFeature{}, false
}

// SupportsContextFeatures reports whether the harness has a startup-context
// surface tclaude can trim. This is Claude Code's feature set; Codex and
// OpenCode expose no equivalent switches, so callers must not emit anything for
// them — and must hide the affordance.
//
// Gated on the harness NAME rather than a capability func for the same reason
// SupportsAutoMemory is: these are plain environment variables and settings
// keys, not lifecycle commands with a per-harness implementation to probe.
func (h *Harness) SupportsContextFeatures() bool {
	return h != nil && h.Name == DefaultName
}

// CanContextFeatures is the UI-side predicate a spawn/profile control gates on
// (mirrors CanAutoMemory).
func (h *Harness) CanContextFeatures() bool {
	return h.SupportsContextFeatures()
}

// ValidateContextFeatureState normalizes one state value. A blank or "default"
// state means "no override" and normalizes to "", which callers drop.
func ValidateContextFeatureState(state string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", ContextFeatureDefault:
		return "", nil
	case ContextFeatureOn:
		return ContextFeatureOn, nil
	case ContextFeatureOff:
		return ContextFeatureOff, nil
	default:
		return "", fmt.Errorf("unsupported context-feature state %q (want %s, %s or %s)",
			state, ContextFeatureOn, ContextFeatureOff, ContextFeatureDefault)
	}
}

// ResolveContextFeatures is the entry point every spawn boundary (daemon spawn,
// `tclaude agent spawn`, `tclaude session new`, profile save, template deploy)
// routes a requested feature map through.
//
// It validates every slug against the catalog and every state against the three
// allowed values, drops default/blank entries so the stored map holds only real
// overrides, and returns nil for "no overrides" so a profile field round-trips
// as unset. Requesting any override for a harness with no steerable
// startup-context surface is an error rather than a silent drop, so a mistake
// surfaces at the spawn boundary instead of vanishing at runtime — the same
// contract ResolveAutoMemory applies.
func ResolveContextFeatures(h *Harness, requested map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for rawSlug, rawState := range requested {
		slug := strings.ToLower(strings.TrimSpace(rawSlug))
		if slug == "" {
			return nil, fmt.Errorf("context feature: empty slug")
		}
		if _, ok := LookupContextFeature(slug); !ok {
			return nil, fmt.Errorf("unknown context feature %q (see `tclaude agent spawn --help` for the catalog)", slug)
		}
		state, err := ValidateContextFeatureState(rawState)
		if err != nil {
			return nil, fmt.Errorf("context feature %q: %w", slug, err)
		}
		if state == "" {
			continue
		}
		out[slug] = state
	}
	if len(out) == 0 {
		return nil, nil
	}
	if !h.CanContextFeatures() {
		return nil, fmt.Errorf("harness %q has no steerable startup-context features "+
			"(context trimming is a Claude Code feature; not available for this harness)", harnessName(h))
	}
	return out, nil
}

// ContextFeatureEnv renders the environment variables a resolved feature map
// injects. Only env-backed catalog entries appear here; a settings-only feature
// is delivered by ContextFeatureSettings instead.
//
// The two directions are deliberately asymmetric:
//
//   - OFF sets the variable to "1", the value every CLAUDE_CODE_DISABLE_* switch
//     recognizes as "disabled".
//   - ON sets it to the EMPTY string rather than omitting it, so an operator who
//     exported CLAUDE_CODE_DISABLE_ARTIFACT=1 in their own shell cannot silently
//     override an agent that asked to keep the feature. Empty is chosen over "0"
//     because it reads as "not disabled" under both plausible implementations —
//     a truthiness test and an equality test against "1" — whereas "0" is only
//     safe under the latter. (AutoMemoryEnvVar uses "0" because Claude Code
//     documents that variable's force-enable value explicitly; these are
//     undocumented, so the weaker assumption is the right one.)
//   - DEFAULT emits nothing at all, leaving the operator's own environment and
//     settings in charge.
func ContextFeatureEnv(features map[string]string) map[string]string {
	out := map[string]string{}
	for slug, state := range features {
		f, ok := LookupContextFeature(slug)
		if !ok || f.EnvVar == "" {
			continue
		}
		switch state {
		case ContextFeatureOff:
			out[f.EnvVar] = "1"
		case ContextFeatureOn:
			out[f.EnvVar] = ""
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ContextFeatureSettings renders the settings.json keys a resolved feature map
// contributes to the per-session `--settings` payload. Only settings-only
// catalog entries appear here.
//
// Both directions are emitted explicitly (true = disabled, false = kept), for
// the same reason ContextFeatureEnv writes an explicit ON: the payload merges
// OVER the operator's settings.json, so an explicit false is what lets a
// per-spawn "keep it" beat an operator-level disable.
func ContextFeatureSettings(features map[string]string) map[string]bool {
	out := map[string]bool{}
	for slug, state := range features {
		f, ok := LookupContextFeature(slug)
		if !ok || f.SettingsKey == "" {
			continue
		}
		switch state {
		case ContextFeatureOff:
			out[f.SettingsKey] = true
		case ContextFeatureOn:
			out[f.SettingsKey] = false
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FormatContextFeatures renders a resolved map as a stable, human-readable
// summary ("artifact=off, bundled-skills=off") for CLI output, log lines and
// spawn notes. Sorted by slug so the output is deterministic.
func FormatContextFeatures(features map[string]string) string {
	if len(features) == 0 {
		return ""
	}
	slugs := make([]string, 0, len(features))
	for slug := range features {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	parts := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		parts = append(parts, slug+"="+features[slug])
	}
	return strings.Join(parts, ", ")
}

// ParseContextFeatures parses the CLI spelling of a feature map: a
// comma-separated list of `slug=state` pairs, where a bare `slug` means off
// (the overwhelmingly common intent — the operator is trimming). It is
// deliberately lenient about whitespace and case, and strict about content:
// ResolveContextFeatures still has the final say on slugs and states.
//
//	--context-features bundled-skills,artifact,git-instructions
//	--context-features bundled-skills=off,artifact=on
func ParseContextFeatures(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	for chunk := range strings.SplitSeq(raw, ",") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		slug, state := chunk, ContextFeatureOff
		if before, after, found := strings.Cut(chunk, "="); found {
			slug = strings.TrimSpace(before)
			state = strings.TrimSpace(after)
		}
		slug = strings.ToLower(slug)
		if slug == "" {
			return nil, fmt.Errorf("context features: %q has no feature name", chunk)
		}
		if prior, dup := out[slug]; dup && prior != state {
			return nil, fmt.Errorf("context feature %q is listed twice with different states", slug)
		}
		out[slug] = state
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ContextFeatureSlugs lists every catalog slug in presentation order — the
// help-text and shell-completion source.
func ContextFeatureSlugs() []string {
	out := make([]string, 0, len(contextFeatureCatalog))
	for _, f := range contextFeatureCatalog {
		out = append(out, f.Slug)
	}
	return out
}
