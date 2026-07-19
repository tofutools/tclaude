package harness

import (
	"fmt"
	"strings"
)

// Codex approval policies — openai/codex `AskForApproval` exposed as the
// `--ask-for-approval` CLI value enum (kebab-case), verified firsthand
// against rust-v0.139.0 (utils/cli/src/approval_mode_cli_arg.rs;
// tui/src/cli.rs `--ask-for-approval`/`-a`). See JOH-167 for the oversight
// research.
//
//   - never        : never ask the user; execution failures return to the
//     model. The no-human-prompt default for an unattended/detached pane
//     (JOH-200). It may automatically execute more in-sandbox commands than
//     prompt-oriented modes, so it is not the least-authority lattice member.
//   - on-request   : the model decides when to ask (Codex's own default) —
//     escalates to a human, so it deadlocks an unattended pane.
//   - on-failure   : DEPRECATED upstream; still escalates on failure.
//   - untrusted    : escalates for any non-trusted command.
//
// `--full-auto` is NOT used: it was removed at rust-v0.139.0 in favour of
// `--sandbox workspace-write` (which JOH-192 already emits) — the deprecation
// warning literally says so.
const (
	ApprovalUntrusted = "untrusted"
	ApprovalOnFailure = "on-failure"
	ApprovalOnRequest = "on-request"
	ApprovalNever     = "never"
)

// codexApproval is Codex's ApprovalCatalog. The default is `never`: a
// tclaude-spawned Codex agent runs detached in tmux with no human at its TUI,
// so any approval prompt that escalates to a human blocks forever (the
// unattended-agent deadlock — JOH-167 §E, JOH-200). `never` is safe precisely
// because the agent is sandboxed by default (JOH-192 workspace-write: writes
// confined to cwd+/tmp, network denied), so "don't ask, just run, return
// failures to the model" cannot escape the sandbox.
type codexApproval struct{}

func (codexApproval) DefaultPolicy() string { return ApprovalNever }

func (codexApproval) ValidatePolicy(policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	if policy == "" {
		return policy, nil
	}
	for _, mode := range codexApprovalModes {
		if policy == mode {
			return policy, nil
		}
	}
	return "", fmt.Errorf("invalid codex approval policy %q (want %s)",
		policy, strings.Join(codexApprovalModes, "|"))
}

// codexApprovalModes is the canonical ordered policy set shared by validation,
// the CLI/profile API, and dashboard selectors. Keep never first: it is the
// daemon launch default for detached agents, so an empty legacy profile renders
// an explicit effective choice instead of a blank control.
var codexApprovalModes = []string{
	ApprovalNever, ApprovalUntrusted, ApprovalOnFailure, ApprovalOnRequest,
}

// Modes returns a fresh copy so callers cannot mutate the validation source.
func (codexApproval) Modes() []string { return append([]string(nil), codexApprovalModes...) }

// The "⚠" prefixes the caveat half of the copy, matching the Claude permission
// modes: the spawn UI collapses mode help behind a [?] but keeps everything
// from the ⚠ onward visible, so a mode that can strand a detached agent still
// says so without the operator opening anything. The default carries none.
var codexApprovalModeHelp = map[string]string{
	ApprovalNever:     "Never request approval; commands allowed by the sandbox run automatically and failures return to the model. Recommended for detached agents.",
	ApprovalUntrusted: "Ask before commands outside Codex's trusted set. ⚠ A detached agent can block waiting for a human unless auto-review is enabled.",
	ApprovalOnFailure: "Deprecated upstream: run in the sandbox, then ask to retry outside it after failure. ⚠ A detached agent can block waiting for a human.",
	ApprovalOnRequest: "Let the model request approval when it wants to run outside the sandbox. ⚠ A detached agent can block waiting for a human unless auto-review is enabled.",
}

func (codexApproval) ModeHelp(policy string) string {
	return codexApprovalModeHelp[strings.TrimSpace(policy)]
}

// Codex auto-review (guardian) — the orthogonal "who answers an approval
// prompt" axis. `--ask-for-approval` decides WHEN Codex asks; this config
// decides WHO decides: the human (`user`, Codex's default) or a guardian
// subagent that auto-decides in the human's place (`auto_review`). Source-
// present but undocumented/experimental at rust-v0.139.0 (config key
// `approvals_reviewer`, set via `-c approvals_reviewer=auto_review`; the value
// has a legacy alias `guardian_subagent`). The guardian fail-closes to Denied
// on timeout/error/malformed and has a per-turn circuit breaker; `/approve`
// is the human override. See JOH-167 and JOH-200 part 2.
//
// It is plumbed as a per-spawn opt-in bool (SpawnSpec.AutoReview) rather than
// a free-text config because tclaude only ever wants the canonical
// `auto_review` value; the bool keeps the harness-agnostic layers from
// knowing Codex's config syntax — codexSpawner is the only place that emits
// the `-c approvals_reviewer="auto_review"` override.
const (
	codexApprovalsReviewerKey  = "approvals_reviewer"
	codexApprovalsReviewerAuto = "auto_review"
)
