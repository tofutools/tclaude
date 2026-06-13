package harness

import (
	"fmt"
	"strings"
)

// Codex approval policies — openai/codex `AskForApproval` exposed as the
// `--ask-for-approval` CLI value enum (kebab-case), verified firsthand
// against rust-v0.139.0 (utils/cli/src/approval_mode_cli_arg.rs;
// tui/src/cli.rs `--ask-for-approval`/`-a`). See
// docs/plans/harness-independence.md §E (JOH-167) for the oversight research.
//
//   - never        : never ask the user; execution failures return to the
//                    model. The ONLY non-escalating posture — the one safe
//                    default for an unattended/detached pane (JOH-200).
//   - on-request   : the model decides when to ask (Codex's own default) —
//                    escalates to a human, so it deadlocks an unattended pane.
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
	switch policy {
	case "", ApprovalUntrusted, ApprovalOnFailure, ApprovalOnRequest, ApprovalNever:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid codex approval policy %q (want %s|%s|%s|%s)",
			policy, ApprovalUntrusted, ApprovalOnFailure, ApprovalOnRequest, ApprovalNever)
	}
}
