package harness

import (
	"fmt"
	"slices"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// codexSpawner builds the `codex` invocation that runs inside the tmux
// pane (JOH-154). The shape mirrors claudeSpawner but for Codex CLI's
// command model — most notably resume is a SUBCOMMAND (`codex resume
// <id>`), not a flag like Claude Code's `claude --resume <id>`.
//
// Like the CC spawner it stays pure (string in → string out) so the
// "unset omits the flag" guarantee is unit-testable without tmux, and it
// shell-quotes everything handed to `sh -c`.
type codexSpawner struct{}

func (codexSpawner) Binary() string { return "codex" }

// BuildCommand assembles the Codex invocation: env exports + the binary,
// the `resume <id>` subcommand when resuming, an optional
// `--dangerously-bypass-hook-trust`, an optional `--sandbox <mode>`, an
// optional `--model`, then any post-`--` passthrough args.
//
// Working directory is NOT passed via `-C/--cd`: tclaude launches the
// pane with `tmux new-session -c <cwd>`, and Codex uses the pane's cwd —
// the same way the CC spawner relies on tmux for cwd. Effort maps onto
// Codex's reasoning-effort config (JOH-155). Sandbox mode is a per-spawn
// `--sandbox` flag (JOH-192) — resolved/validated at the spawn boundary
// (ResolveSandboxMode) and emitted verbatim here, so the user's config.toml
// sandbox_mode/profiles stay untouched.
func (codexSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "codex"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
	}
	cmd := spec.EnvExports + binary
	if spec.ResumeID != "" {
		// `codex resume <id>` — resume is a subcommand; the id is a
		// positional. Quoted defensively even though it's a UUID.
		cmd += " resume " + clcommon.ShellQuoteArg(spec.ResumeID)
	}
	if spec.BypassHookTrust {
		// Run configured hooks without persisted hook trust for this
		// invocation — a headless escape hatch (default off). Accepted
		// both on a fresh `codex` and on `codex resume <id>`, like
		// `--model`. No value, so nothing to quote.
		cmd += " --dangerously-bypass-hook-trust"
	}
	if spec.PermissionProfile != "" {
		// `-p <name>` layers $CODEX_HOME/<name>.config.toml, whose
		// default_permissions activates a permissions profile for THIS spawn
		// only (the TUI/exec have no `-P` flag; selection is via
		// default_permissions). tclaude uses this INSTEAD of `--sandbox` for a
		// daemon-spawned agent that must reach the agentd socket: Codex ignores
		// permission profiles whenever a `--sandbox`/sandbox_mode is present,
		// and only the profile model can allowlist that one Unix socket
		// (JOH-207). Mutually exclusive with SandboxMode (the spec builder sets
		// one or the other) — emitting both would let `--sandbox` silently void
		// the profile, so the profile wins and `--sandbox` is omitted. Accepted
		// on both a fresh `codex` and `codex resume <id>` (shared option,
		// verified against codex-cli 0.139.0). The value is a validated profile
		// name, never free text, but quoted defensively.
		cmd += " -p " + clcommon.ShellQuoteArg(spec.PermissionProfile)
	} else if spec.SandboxMode != "" {
		// `--sandbox {read-only|workspace-write|danger-full-access}` selects
		// Codex's OS-native sandbox for THIS invocation only — a per-spawn
		// flag, so the user's config.toml sandbox_mode/profiles are left
		// untouched. Accepted on both a fresh `codex` and `codex resume
		// <id>` (shared option). The value is a validated enum
		// (codexSandbox.ValidateMode), never free text, but quoted defensively.
		cmd += " --sandbox " + clcommon.ShellQuoteArg(spec.SandboxMode)
	}
	if spec.ApprovalPolicy != "" {
		// `--ask-for-approval {untrusted|on-failure|on-request|never}` (the
		// short is `-a`) selects Codex's approval policy for THIS invocation
		// only — a per-spawn flag, so the user's config.toml/profiles stay
		// untouched. Accepted on both a fresh `codex` and `codex resume <id>`
		// (resume flattens the same TuiCli, verified against
		// rust-v0.139.0). The daemon resolves this to `never` for an
		// unattended pane so it never blocks on a prompt no human can answer
		// (JOH-200). The value is a validated enum (codexApproval.ValidatePolicy),
		// never free text, but quoted defensively.
		cmd += " --ask-for-approval " + clcommon.ShellQuoteArg(spec.ApprovalPolicy)
	}
	if spec.AutoReview {
		// `-c approvals_reviewer="auto_review"` routes approval prompts to
		// Codex's guardian subagent (auto-decides in the human's place)
		// instead of the human (`user`, the default) — the orthogonal "who
		// answers" axis to --ask-for-approval's "when to ask". A per-spawn
		// `-c` config override, so the user's config.toml stays untouched.
		// Accepted on both a fresh `codex` and `codex resume <id>`. The value
		// is a TOML-quoted string (matching Codex's own `-c model="o3"`
		// convention) and the whole `key="value"` is shell-quoted as one arg.
		// Experimental/undocumented upstream, hence opt-in (JOH-200 part 2).
		cmd += " -c " + clcommon.ShellQuoteArg(codexApprovalsReviewerKey+`="`+codexApprovalsReviewerAuto+`"`)
	}
	if len(spec.ShellEnvironment) > 0 {
		// Codex may build tool-command environments from a saved user-shell
		// snapshot. Pin sandbox-profile values in the documented "always wins"
		// layer as well as exporting them into the Codex process, otherwise a
		// profile assignment such as GOBIN can replace an agent-owned binding.
		// Sort the map so launch commands and their tests stay deterministic.
		names := make([]string, 0, len(spec.ShellEnvironment))
		for name := range spec.ShellEnvironment {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			override := "shell_environment_policy.set." + name + "=" + codexTOMLString(spec.ShellEnvironment[name])
			cmd += " -c " + clcommon.ShellQuoteArg(override)
		}
	}
	if spec.Model != "" {
		// `--model` is accepted both on a fresh `codex` and on
		// `codex resume <id>` (shared option).
		cmd += " --model " + clcommon.ShellQuoteArg(spec.Model)
	}
	if spec.Effort != "" {
		// Codex has no `--effort` flag; reasoning effort is a config
		// value, set via `-c model_reasoning_effort=…`. The value is a
		// TOML-quoted string (matching Codex's own `-c model="o3"`
		// convention) and the whole `key="value"` is shell-quoted as one
		// arg. spec.Effort is a validated tclaude level; codexReasoningEffort
		// maps it onto the selected Codex model's scale.
		cmd += " -c " + clcommon.ShellQuoteArg(`model_reasoning_effort="`+codexReasoningEffort(spec.Model, spec.Effort)+`"`)
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	// `codex [OPTIONS] [PROMPT]` — a trailing positional the interactive TUI
	// submits itself at launch (verified against codex-cli 0.139.0:
	// "[PROMPT]  Optional user prompt to start the session"). This is how a
	// Codex spawn takes its first turn without a human keystroke, so its
	// conv-id materialises (JOH-205) — Codex self-submits, so the prompt
	// queues safely behind any startup modal (dir-trust / hooks / auth) and
	// tclaude never has to send-keys an unconfirmed pane. Only on a FRESH
	// launch: `codex resume <id>` continues an existing conversation whose
	// id is already known, so it needs no seed (and resume's positional-
	// prompt handling differs). Quoted as a single arg so the whole prompt
	// is one [PROMPT], never split into stray flags/words.
	if spec.InitialPrompt != "" && spec.ResumeID == "" {
		cmd += " " + clcommon.ShellQuoteArg(spec.InitialPrompt)
	}
	return cmd
}

// codexTOMLString renders an arbitrary validated sandbox-profile environment
// value as a TOML basic string for Codex's -c parser. Profile values may carry
// whitespace and control characters other than NUL, so escaping only quotes
// and backslashes is insufficient here.
func codexTOMLString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexModels is the ModelCatalog for Codex (JOH-154/155). It offers a small
// curated set of current Codex models while still accepting models outside the
// list: Codex owns the authoritative, per-release validation. It also rejects
// Claude Code slugs and accepts tclaude's effort levels, which codexSpawner
// maps onto Codex's reasoning-effort scale.
type codexModels struct{}

// codexKnownModels is deliberately a suggestion list, not an allow-list.
// Keeping the current first-party choices here gives every ModelCatalog-driven
// surface (spawn, profiles, roles, and template-local launch profiles) the same
// dropdown while ValidateModel continues to pass future/custom OpenAI IDs
// through to Codex.
var codexKnownModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex-spark",
}

// ValidateModel rejects a Claude Code model slug/ID chosen for a Codex
// session (a clear error beats forwarding e.g. "opus" or "claude-fable-5"
// to `codex --model`, which fails opaquely at launch). Any other non-empty
// value passes through trimmed: Codex's model set changes per release and
// Codex validates it itself, so tclaude doesn't curate a list that would
// go stale. Empty stays empty → the spawner omits `--model`.
func (codexModels) ValidateModel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// clcommon.IsValidModel recognises CC aliases (opus, sonnet, …) and
	// claude-* full IDs; fold case the way clcommon.ValidateModel does.
	if clcommon.IsValidModel(strings.ToLower(s)) {
		return "", fmt.Errorf("%q is a Claude Code model; the codex harness uses OpenAI models (e.g. gpt-5, gpt-5-codex)", s)
	}
	return s, nil
}

// ValidateEffort accepts tclaude's effort levels — a harness-agnostic
// concept — validating them exactly as Claude Code does. The level →
// Codex reasoning-effort mapping is applied by codexSpawner.BuildCommand
// when it emits the config override (see codexReasoningEffort).
func (codexModels) ValidateEffort(s string) (string, error) {
	return clcommon.ValidateEffort(s)
}

// Models returns a copy of the curated suggestions. ValidateModel remains the
// authority and accepts custom OpenAI IDs outside this list.
func (codexModels) Models() []string       { return slices.Clone(codexKnownModels) }
func (codexModels) EffortLevels() []string { return slices.Clone(clcommon.ValidEffortLevels) }

// codexReasoningEffort maps a validated tclaude effort level onto the selected
// Codex model's scale. GPT-5.6 has a distinct max level, while older models top
// out at xhigh; all lower shared levels pass through unchanged. An unset/custom
// model retains the backwards-compatible max → xhigh mapping because tclaude
// cannot know whether that model accepts the newer level.
func codexReasoningEffort(model, effort string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	isGPT56 := model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-")
	if effort == "max" && !isGPT56 {
		return "xhigh"
	}
	return effort // low / medium / high / xhigh (and GPT-5.6 max) map 1:1
}
