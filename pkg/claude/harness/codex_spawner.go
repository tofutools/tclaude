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
	cmd := spec.EnvExports + "codex"
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
	if spec.SandboxMode != "" {
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
		// maps it onto Codex's scale.
		cmd += " -c " + clcommon.ShellQuoteArg(`model_reasoning_effort="`+codexReasoningEffort(spec.Effort)+`"`)
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	return cmd
}

// codexModels is the ModelCatalog for Codex (JOH-154/155). It validates a
// model is not a Claude Code slug (otherwise pass-through, since Codex
// validates its own per-release model set) and accepts tclaude's effort
// levels, which codexSpawner maps onto Codex's reasoning-effort scale.
type codexModels struct{}

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

// Models returns no curated suggestions — Codex's model set changes per
// release, so ValidateModel (reject-CC-slug, else pass-through) is the
// authority. EffortLevels returns tclaude's levels, now valid for Codex.
func (codexModels) Models() []string       { return nil }
func (codexModels) EffortLevels() []string { return slices.Clone(clcommon.ValidEffortLevels) }

// codexReasoningEffort maps a validated tclaude effort level onto Codex's
// reasoning-effort scale. Codex's ReasoningEffort is
// none/minimal/low/medium/high/xhigh; tclaude's low/medium/high/xhigh pass
// straight through, and "max" — which Codex has no separate level for —
// maps to Codex's highest, "xhigh".
func codexReasoningEffort(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort // low / medium / high / xhigh map 1:1
}
