package harness

import (
	"fmt"
	"strings"
)

// ApprovalLineageAllowed reports whether a child approval posture has no
// broader AUTOMATIC command-acceptance capability than its parent.
//
// Both sides are first resolved to a normalized capability shape and then
// compared as a subset test. There are no per-direction or per-harness
// exceptions: Codex approval policies and Claude Code permission modes are
// projected onto the SAME capability axes, because their labels do not form one
// directly comparable authority lattice (see TCL-92).
//
// Human approval is baseline throughout: a human remains the trust root, so a
// posture that reaches a human — the Claude approval popup, a Codex escalation
// prompt, the operator's own allow/deny rules — grants the agent no automatic
// capability of its own. What the gate guards is what an agent can cause to
// happen WITHOUT a human: automatic in-sandbox execution, approval by a machine
// reviewer instead of a person, and unreviewed blanket approval.
func ApprovalLineageAllowed(parentHarness, parentPolicy string, parentAutoReview bool, childHarness, childPolicy string, childAutoReview bool) bool {
	// Preserve the operator's live Claude posture across an exact inherit →
	// inherit continuation. This is intentionally the sole uncertainty
	// exception: it keeps the ordinary recursive Claude workflow usable without
	// crediting an inherit parent with any explicit automatic capability it has
	// not proved. Any different child posture still goes through the dual-bound
	// comparison below (or must be launched by the human trust root).
	if normalizeLineageHarness(parentHarness) == DefaultName &&
		normalizeLineageHarness(childHarness) == DefaultName &&
		strings.TrimSpace(parentPolicy) == claudePermInherit &&
		strings.TrimSpace(childPolicy) == claudePermInherit &&
		!parentAutoReview && !childAutoReview {
		return true
	}
	parent := classifyParentApprovalLineage(parentHarness, parentPolicy, parentAutoReview)
	child := classifyChildApprovalLineage(childHarness, childPolicy, childAutoReview)
	if !parent.valid || !child.valid {
		return false
	}
	return child.capability&^parent.capability == 0
}

// ApprovalLineageDenialHint returns actionable guidance for a denied child
// posture, or "" when no specific guidance applies. It exists so the spawn
// guard can tell a caller HOW to succeed rather than only that it failed —
// notably for the unresolvable `inherit` child, whose effective posture cannot
// be proven and therefore fails closed.
//
// The suggestion is computed against the PARENT's own capability, so it can
// only ever name a mode that would actually be allowed. Suggesting a mode that
// earns a second 403 is worse than saying nothing.
func ApprovalLineageDenialHint(parentHarness, parentPolicy string, parentAutoReview bool, childHarness, childPolicy string) string {
	switch normalizeLineageHarness(childHarness) {
	case CopilotName:
		if strings.TrimSpace(childPolicy) != CopilotApprovalInherit {
			// `allow-tools` is the only other token, and a parent that cannot
			// delegate it cannot delegate anything Copilot offers. There is no
			// narrower Copilot posture to point at, so say nothing rather than
			// invent advice — the guard's own message already states the two
			// postures involved.
			return ""
		}
		hint := fmt.Sprintf("the child requested %q, which emits no permission flags, so its posture is decided by the operator's Copilot configuration and by answers remembered in-pane; it cannot be proven at spawn time and is therefore treated as the broadest posture", CopilotApprovalInherit)
		if ApprovalLineageAllowed(parentHarness, parentPolicy, parentAutoReview,
			CopilotName, CopilotApprovalAllowTools, false) {
			return hint + fmt.Sprintf("; pass %q to spawn a child whose posture tclaude renders and records itself", CopilotApprovalAllowTools)
		}
		return hint + "; this parent cannot delegate any provable Copilot posture, so a human must spawn this child"
	case OpenCodeName:
		if strings.TrimSpace(childPolicy) != OpenCodeApprovalAllowTools {
			return ""
		}
		for _, mode := range []string{OpenCodeApprovalAsk, OpenCodeApprovalDeny} {
			if ApprovalLineageAllowed(parentHarness, parentPolicy, parentAutoReview,
				OpenCodeName, mode, false) {
				return fmt.Sprintf("%q automatically accepts scoped edits; pass %q for a human-gated, non-escalating child posture",
					OpenCodeApprovalAllowTools, mode)
			}
		}
		return fmt.Sprintf("%q automatically accepts scoped edits and can only be minted by a parent that already holds that capability, or by a human",
			OpenCodeApprovalAllowTools)
	case DefaultName:
	default:
		return ""
	}
	switch strings.TrimSpace(childPolicy) {
	case claudePermInherit:
		hint := fmt.Sprintf("the child requested %q, whose effective posture is decided by the operator's settings and cannot be proven at spawn time, so it is treated as the broadest non-bypass posture", claudePermInherit)
		if alt := widestAllowedClaudeChildMode(parentHarness, parentPolicy, parentAutoReview); alt != "" {
			return hint + fmt.Sprintf("; pass an explicit permission mode such as %q to spawn a child with a provable posture", alt)
		}
		// Nothing this parent could pass instead; do not invent advice.
		return hint + "; this parent cannot delegate any provable Claude posture, so a human must spawn this child"
	case claudePermBypass:
		return fmt.Sprintf("%q removes every approval guardrail and can only be minted by a parent that already holds it, or by a human", claudePermBypass)
	default:
		return ""
	}
}

// widestAllowedClaudeChildMode returns the most autonomous explicit Claude
// permission mode this parent may actually delegate, or "" if it may delegate
// none. Ordered most- to least-autonomous so the caller is pointed at the mode
// that will serve a detached agent best.
func widestAllowedClaudeChildMode(parentHarness, parentPolicy string, parentAutoReview bool) string {
	for _, mode := range []string{claudePermAuto, claudePermAccept, claudePermDefault, claudePermPlan} {
		if ApprovalLineageAllowed(parentHarness, parentPolicy, parentAutoReview, DefaultName, mode, false) {
			return mode
		}
	}
	return ""
}

// The capability axes an approval posture is projected onto. They are bits, not
// a total order, because these capabilities are genuinely incomparable: a Codex
// guardian reviewer and Claude's in-sandbox classifier are different powers, and
// neither implies the other.
type approvalAutoCapability uint8

const (
	// approvalAutoBaseline is "every non-read-only action is gated by a human,
	// by the operator's own pre-approved rules, or denied outright". Claude
	// plan/default/dontAsk and Codex untrusted all land here: they can reach a
	// human, but the agent itself accepts nothing automatically.
	approvalAutoBaseline approvalAutoCapability = 0

	// approvalAutoEdits is "may write files and run common filesystem commands
	// in its working directory automatically, with no human in the loop". Claude
	// acceptEdits holds exactly this and no more: every non-edit action still
	// prompts.
	approvalAutoEdits approvalAutoCapability = 1 << 0

	// approvalAutoCommands is "may run ARBITRARY commands automatically, with no
	// human in the loop, while they stay inside the agent's sandbox". Codex
	// never/on-request/on-failure hold it, as does Claude auto (its supervisor
	// approves safe commands, not only edits).
	//
	// This is deliberately distinct from approvalAutoEdits. Collapsing the two
	// would let an acceptEdits parent — which must ask a human before every
	// non-edit command — mint a child that runs `curl`, `git push`, or `rm -rf`
	// unattended. Anything holding it also holds approvalAutoEdits.
	approvalAutoCommands approvalAutoCapability = 1 << 1

	// approvalAutoReviewer is "a machine reviewer may approve, in a human's
	// place, actions that would otherwise escalate past the sandbox boundary".
	// Codex Auto-review holds it. Claude's `auto` classifier does NOT: per
	// TCL-92 it reviews and tightens in-sandbox operations and is not a
	// boundary-escalation grant.
	approvalAutoReviewer approvalAutoCapability = 1 << 2

	// approvalAutoUnreviewed is "auto-approve everything, with no reviewer of
	// any kind". Only Claude bypassPermissions holds it.
	approvalAutoUnreviewed approvalAutoCapability = 1 << 3

	// approvalAutoInSandbox is the full unattended in-sandbox execution shape.
	approvalAutoInSandbox = approvalAutoEdits | approvalAutoCommands
)

type approvalLineagePosture struct {
	capability approvalAutoCapability
	valid      bool
}

// classifyParentApprovalLineage returns the capability floor that the caller is
// PROVEN to hold. classifyChildApprovalLineage returns the capability ceiling
// the requested child could receive. The distinction is load-bearing for
// Claude's `inherit`: the operator's live settings are unknown, so treating the
// same unknown as broad on both sides lets an inherit parent mint authority it
// may not have. A lower-bound parent and upper-bound child make uncertainty
// fail closed without inventing a false total order between the harnesses.
func classifyParentApprovalLineage(harnessName, policy string, autoReview bool) approvalLineagePosture {
	return classifyApprovalLineage(harnessName, policy, autoReview, false)
}

func classifyChildApprovalLineage(harnessName, policy string, autoReview bool) approvalLineagePosture {
	return classifyApprovalLineage(harnessName, policy, autoReview, true)
}

func classifyApprovalLineage(harnessName, policy string, autoReview bool, child bool) approvalLineagePosture {
	policy = strings.TrimSpace(policy)
	switch normalizeLineageHarness(harnessName) {
	case DefaultName:
		// Claude Code has no separate reviewer flag; auto-review is a Codex-only
		// axis, so a Claude posture carrying it is malformed. Fail closed rather
		// than silently ignoring a toggle the caller believed was applied.
		if autoReview {
			return approvalLineagePosture{}
		}
		switch policy {
		case claudePermPlan, claudePermDefault, claudePermDontAsk:
			// plan is read-only; default prompts for everything; dontAsk
			// auto-DENIES anything not pre-approved. None accepts automatically.
			return approvalLineagePosture{capability: approvalAutoBaseline, valid: true}
		case claudePermAccept:
			// Edits and common fs commands in the cwd only — every other action
			// still prompts a human.
			return approvalLineagePosture{capability: approvalAutoEdits, valid: true}
		case claudePermAuto:
			// A supervisor model approves safe actions, including commands. It
			// tightens what runs inside the sandbox and cannot escalate past it.
			return approvalLineagePosture{capability: approvalAutoInSandbox, valid: true}
		case claudePermInherit:
			// `inherit` means "whatever the operator's settings.json decides,
			// plus the agentd approval popup". That is unknowable at spawn time.
			// A parent is therefore credited only with the baseline capability it
			// certainly has, while a child is charged the broadest non-bypass
			// capability it could receive. This dual bound prevents an unknown
			// parent from becoming a delegation grant and makes an unknown child
			// require a real human trust-root decision.
			capability := approvalAutoBaseline
			if child {
				capability = approvalAutoInSandbox | approvalAutoReviewer
			}
			return approvalLineagePosture{capability: capability, valid: true}
		case claudePermBypass:
			return approvalLineagePosture{capability: approvalAutoInSandbox | approvalAutoReviewer | approvalAutoUnreviewed, valid: true}
		default:
			// Blank is an old/direct-session sentinel. It might represent any
			// historic explicit mode, so do not treat it as known inherit.
			return approvalLineagePosture{}
		}
	case CodexName:
		switch policy {
		case ApprovalUntrusted, ApprovalOnFailure, ApprovalOnRequest, ApprovalNever:
			capability := approvalAutoBaseline
			// `untrusted` asks before every command outside Codex's trusted set.
			// The other policies may run commands automatically while they stay
			// inside the OS sandbox.
			if policy != ApprovalUntrusted {
				capability |= approvalAutoInSandbox
			}
			// `never` produces no approval requests, so enabling the reviewer
			// alongside it grants no reviewer capability.
			if autoReview && policy != ApprovalNever {
				capability |= approvalAutoReviewer
			}
			return approvalLineagePosture{capability: capability, valid: true}
		default:
			return approvalLineagePosture{}
		}
	case CopilotName:
		// Copilot has no guardian subagent; auto-review is a Codex-only axis.
		if autoReview {
			return approvalLineagePosture{}
		}
		switch policy {
		case CopilotApprovalAllowTools:
			// --allow-all-tools accepts every tool call automatically, and the
			// measured gate is per-COMMAND risk classification rather than a
			// tool allowlist, so this is arbitrary command execution with no
			// human in the loop: approvalAutoInSandbox, the same shape as Codex
			// `never` and Claude `auto`.
			//
			// "InSandbox" names the capability, not a guarantee about Copilot —
			// whether anything actually confines those commands is the sandbox
			// axis's business, and it has its own lineage guard. Reading this
			// bit as a containment claim would be a mistake in every harness,
			// but especially this one: Copilot's built-in edits are not
			// OS-confined at all.
			return approvalLineagePosture{capability: approvalAutoInSandbox, valid: true}
		case CopilotApprovalInherit:
			// The same dual bound Claude's `inherit` uses, for the same reason
			// and then some. A Copilot launch with no permission flags is
			// decided by settings.json/config.json plus whatever the operator
			// answered in-pane and told Copilot to remember — the folder-trust
			// dialog's option 2 is literally "remember this folder for future
			// sessions", and allowedUrls persists the same way. Nothing bounds
			// that state below allow-everything from outside the pane.
			//
			// So a PARENT is credited only the baseline it certainly has, and a
			// CHILD is charged the broadest posture it could turn out to hold.
			// The consequence is deliberate: an `allow-tools` Copilot agent
			// cannot mint an `inherit` child, because it cannot prove that
			// child would be no broader than itself. A human can, and the
			// denial hint says so.
			capability := approvalAutoBaseline
			if child {
				capability = approvalAutoInSandbox | approvalAutoReviewer | approvalAutoUnreviewed
			}
			return approvalLineagePosture{capability: capability, valid: true}
		default:
			// Blank is a pre-catalog Copilot row. It is NOT treated as a known
			// `inherit` even though every such launch did emit zero permission
			// flags: the spawn guard reports an unreconstructable posture with
			// an actionable "relaunch it" message, and quietly minting lineage
			// authority for a row that predates the recording is exactly the
			// widen-on-uncertainty this file exists to prevent.
			return approvalLineagePosture{}
		}
	case OpenCodeName:
		// OpenCode has no guardian; carrying auto-review is malformed even
		// though the catalog itself is otherwise valid.
		if autoReview {
			return approvalLineagePosture{}
		}
		switch policy {
		case OpenCodeApprovalDeny, OpenCodeApprovalAsk:
			return approvalLineagePosture{capability: approvalAutoBaseline, valid: true}
		case OpenCodeApprovalAllowTools:
			// allow-tools may accept built-in edits automatically, but never
			// accepts bash. Network reach is constrained separately by the
			// inherited sandbox profile.
			return approvalLineagePosture{capability: approvalAutoEdits, valid: true}
		default:
			return approvalLineagePosture{}
		}
	default:
		return approvalLineagePosture{}
	}
}

func normalizeLineageHarness(harnessName string) string {
	if harnessName = strings.TrimSpace(harnessName); harnessName == "" {
		return DefaultName
	}
	return harnessName
}
