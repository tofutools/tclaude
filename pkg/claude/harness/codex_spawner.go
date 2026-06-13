package harness

import (
	"fmt"
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
// `--dangerously-bypass-hook-trust`, an optional `--model`, then any
// post-`--` passthrough args.
//
// Working directory is NOT passed via `-C/--cd`: tclaude launches the
// pane with `tmux new-session -c <cwd>`, and Codex uses the pane's cwd —
// the same way the CC spawner relies on tmux for cwd. Sandbox policy and
// reasoning effort are deliberately omitted: their mapping onto tclaude's
// model is a research item (sandbox → JOH-166, reasoning/effort → the M2
// mapping), and codexModels.ValidateEffort rejects a non-empty effort
// today, so spec.Effort is always "" here. Codex falls back to its own
// config defaults for both.
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
	if spec.Model != "" {
		// `--model` is accepted both on a fresh `codex` and on
		// `codex resume <id>` (shared option).
		cmd += " --model " + clcommon.ShellQuoteArg(spec.Model)
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

// codexModels is a minimal ModelCatalog for Codex (JOH-154). Model
// validation is pass-through and effort is rejected-with-guidance until
// the reasoning mapping is settled — enough to make `session new
// --harness codex [--model …]` work without prematurely freezing the
// effort↔reasoning question.
type codexModels struct{}

// ValidateModel passes a non-empty model through (trimmed). Codex's model
// set changes with releases and Codex validates the value itself at
// launch, so tclaude does not curate a list that would go stale; an empty
// value stays empty → the spawner omits `--model` and Codex uses its own
// default.
func (codexModels) ValidateModel(s string) (string, error) {
	return strings.TrimSpace(s), nil
}

// ValidateEffort accepts only the empty value for now. Codex exposes
// reasoning effort via `-c model_reasoning_effort=…`, but its levels
// (minimal/low/medium/high) don't map 1:1 onto tclaude's
// (low/medium/high/xhigh/max) — that mapping is an open M2 question. Until
// it's settled, a non-empty effort errors rather than silently doing
// nothing or guessing a mapping.
func (codexModels) ValidateEffort(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	return "", fmt.Errorf("reasoning effort is not yet wired for the codex harness (open M2 mapping); omit --effort for codex sessions")
}

// Models / EffortLevels return no curated suggestions yet — Codex's model
// set isn't enumerated here and effort is unmapped. Spawn UIs fall back to
// free-form entry; ValidateModel/ValidateEffort remain the authority.
func (codexModels) Models() []string       { return nil }
func (codexModels) EffortLevels() []string { return nil }
