package harness

import (
	"fmt"
	"strings"
)

// ApprovalCatalog is the optional capability for a harness that takes a
// launch-time approval-policy flag (Codex's `--ask-for-approval`). A harness
// whose approval handling is configured out of band — Claude Code, whose
// permission/approval behaviour lives in settings.json, not a launch flag —
// leaves Harness.Approval nil, so the spawn path performs no approval handling
// for it (SupportsApproval() is false; passing a policy is an error the caller
// surfaces).
//
// The contract mirrors SandboxCatalog: name the secure default policy and
// validate/normalize a requested one. "Secure" here means *non-escalating* —
// an unattended/detached pane must never block on an approval prompt no human
// can answer (the deadlock from docs/plans/harness-independence.md §E,
// JOH-167). The default is paired with the sandbox default (JOH-192): non-
// escalating approvals are only safe because writes are sandbox-confined.
type ApprovalCatalog interface {
	// DefaultPolicy is the policy a tclaude-spawned (unattended) agent runs
	// under when the caller didn't choose one. It must be a *non-escalating*
	// policy (Codex: "never"): unspecified means "autonomous by default, so
	// the detached pane can't deadlock".
	DefaultPolicy() string
	// ValidatePolicy normalizes and validates a requested policy. The empty
	// string is returned unchanged (callers substitute DefaultPolicy where a
	// default is wanted, via ResolveApprovalPolicy); any other value is either
	// a recognized policy (returned trimmed) or an error naming the valid set.
	ValidatePolicy(policy string) (string, error)
}

// ResolveApprovalPolicy is the entry point the *daemon* spawn boundaries
// (agentd spawn/resume/clone/reincarnate, `tclaude agent spawn`) use to turn a
// requested approval policy into the value to thread into
// SpawnSpec.ApprovalPolicy. It applies the non-escalating default, because an
// agentd-spawned agent is detached/unattended and must not deadlock on an
// approval prompt:
//
//   - Harness has no launch approval flag (Claude Code): an explicit policy is
//     an error (its approval behaviour is settings.json-driven, not a launch
//     flag); an empty request resolves to "" (omit).
//   - Harness takes one (Codex): an empty request resolves to the secure
//     DefaultPolicy (never); any explicit policy is validated.
//
// requested is trimmed first, so surrounding whitespace never leaks into the
// flag.
func ResolveApprovalPolicy(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" && h.SupportsApproval() {
		requested = h.Approval.DefaultPolicy()
	}
	return ValidateApprovalPolicy(h, requested)
}

// ValidateApprovalPolicy validates a requested policy WITHOUT applying the
// harness default — empty stays empty (omit the flag). It is the direct
// `tclaude session new` path's entry point: the human running session new is
// the trust root and can attach to the pane to answer prompts, so tclaude must
// not silently force a non-escalating policy on them — it only emits
// `--ask-for-approval` when they pass it explicitly (the daemon spawn path
// uses ResolveApprovalPolicy for the non-escalating default instead, since its
// pane is unattended). An explicit policy for a harness with no launch
// approval flag (Claude Code) is still an error.
func ValidateApprovalPolicy(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	if !h.SupportsApproval() {
		return "", fmt.Errorf("harness %q has no launch-time approval policy "+
			"(its approval handling is configured out of band, not via --ask-for-approval)", h.Name)
	}
	return h.Approval.ValidatePolicy(requested)
}
