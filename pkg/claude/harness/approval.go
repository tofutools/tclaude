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
// can answer (the deadlock from JOH-167). The default is paired with the
// sandbox default (JOH-192): non-
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
	// Modes lists the selectable approval/permission policies for spawn UIs —
	// the ApprovalCatalog parallel to SandboxCatalog.Modes, driving the spawn
	// dialog / profile editor's approval <select>. The same set must drive
	// validation so CLI, profiles, and dashboard authoring cannot drift.
	Modes() []string
	// ModeHelp returns a one-line human description of a policy for spawn UIs
	// — notably whether it is safe for an unattended/detached agent — or "" for
	// an unrecognized policy. Like SandboxCatalog.ModeHelp, the copy lives
	// beside the modes it describes so it can't drift from Modes().
	ModeHelp(policy string) string
}

// ResolveApprovalPolicy is the entry point the *daemon* spawn boundaries
// (agentd spawn/resume/clone/reincarnate, `tclaude agent spawn`) use to turn a
// requested approval policy into the value to thread into
// SpawnSpec.ApprovalPolicy. It applies the non-escalating default, because an
// agentd-spawned agent is detached/unattended and must not deadlock on an
// approval prompt:
//
//   - Harness with no approval catalog: an explicit policy is an error; an empty
//     request resolves to "" (omit). Both shipped harnesses HAVE a catalog now —
//     this branch only guards a future harness that leaves Approval nil.
//   - Codex: an empty request resolves to the secure DefaultPolicy (never); any
//     explicit policy is validated.
//   - Claude Code: an empty request resolves to its DefaultPolicy (auto), so an
//     un-chosen Claude spawn launches with `--permission-mode auto` — a known,
//     in-sandbox-bounded posture the detached pane cannot deadlock on. An
//     operator who wants their settings.json posture verbatim selects `inherit`
//     explicitly, which ValidatePolicy carries as a first-class sentinel and
//     the spawner collapses to "omit the flag".
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
// not silently force a posture on them — it emits a value only when they pass
// one explicitly (the daemon spawn path uses ResolveApprovalPolicy for the safe
// default instead, since its pane is unattended). An explicit policy for a
// harness with no approval catalog is still an error (no shipped harness hits
// this — Claude Code now validates its --permission-mode values and normalizes
// inherit to "").
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

// ResolveAutoReview gates the experimental auto-review (guardian) opt-in for a
// spawn: it returns the requested bool to thread into SpawnSpec.AutoReview, or
// an error if auto-review was requested for a harness that has no approvals
// reviewer. Auto-review is part of the approval subsystem (it changes *who*
// answers an approval prompt — a guardian subagent vs the human), so it is
// gated on the SupportsAutoReview() capability (the ApprovalsReviewer flag),
// NOT on SupportsApproval(): a harness can have a launch approval/permission
// catalog yet no guardian to route to. Claude Code is exactly that — it has a
// permission-mode catalog but no reviewer subagent — so `--auto-review` stays
// rejected for it, and silently dropping the flag would hide a mistake. There
// is no non-false default to apply, so — unlike ResolveApprovalPolicy /
// ValidateApprovalPolicy — a single function serves both the daemon spawn path
// and the direct `session new` path: false (off) is the default everywhere,
// and the experimental guardian is only ever engaged by an explicit opt-in.
// See JOH-200 part 2.
func ResolveAutoReview(h *Harness, requested bool) (bool, error) {
	if requested && !h.SupportsAutoReview() {
		return false, fmt.Errorf("harness %q has no approvals reviewer "+
			"(auto-review is a Codex approval-subsystem feature; not available for this harness)", h.Name)
	}
	return requested, nil
}
