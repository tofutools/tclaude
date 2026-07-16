package harness

import "strings"

// ApprovalLineageAllowed reports whether a child approval posture has no
// broader AUTOMATIC command-acceptance capability than its parent. Human
// approval paths are baseline: a human remains the trust root. The policies
// are deliberately not forced into a total order because sandbox-auto and
// classifier/guardian approval are incomparable capability sets.
func ApprovalLineageAllowed(parentHarness, parentPolicy string, parentAutoReview bool, childHarness, childPolicy string, childAutoReview bool) bool {
	parent := classifyApprovalLineage(parentHarness, parentPolicy, parentAutoReview)
	child := classifyApprovalLineage(childHarness, childPolicy, childAutoReview)
	if !parent.valid || !child.valid {
		return false
	}
	parentIsClaude := strings.TrimSpace(parentHarness) == "" || strings.TrimSpace(parentHarness) == DefaultName
	childIsClaude := strings.TrimSpace(childHarness) == "" || strings.TrimSpace(childHarness) == DefaultName
	parentIsClaudeBypass := parentIsClaude && strings.TrimSpace(parentPolicy) == claudePermBypass && !parentAutoReview
	if childIsClaude {
		// Claude Code merges permission rules, hooks, and sandbox settings from
		// files in the child cwd. A parent that can write that cwd can therefore
		// alter the child's effective posture independently of its requested
		// mode. Only an explicitly bypassed parent already holds equivalent
		// approval authority.
		return parentIsClaudeBypass
	}
	if parentIsClaude {
		// Codex automatically executes commands that remain inside its OS
		// sandbox, while non-bypass Claude modes may prompt, deny, or classify
		// those same actions. The sandbox-lineage guard bounds WHERE a child can
		// act; it does not make those approval semantics equivalent.
		return parentIsClaudeBypass
	}
	return child.capability&^parent.capability == 0
}

type approvalAutoCapability uint8

const (
	approvalAutoBaseline   approvalAutoCapability = 0
	approvalAutoSandbox    approvalAutoCapability = 1 << 0
	approvalAutoClassifier approvalAutoCapability = 1 << 1
	approvalAutoAll                               = approvalAutoSandbox | approvalAutoClassifier
)

type approvalLineagePosture struct {
	capability approvalAutoCapability
	valid      bool
}

func classifyApprovalLineage(harnessName, policy string, autoReview bool) approvalLineagePosture {
	harnessName = strings.TrimSpace(harnessName)
	if harnessName == "" {
		harnessName = DefaultName
	}
	policy = strings.TrimSpace(policy)
	switch harnessName {
	case DefaultName:
		if autoReview {
			return approvalLineagePosture{}
		}
		switch policy {
		case claudePermInherit:
			return approvalLineagePosture{capability: approvalAutoBaseline, valid: true}
		case claudePermPlan, claudePermDefault, claudePermDontAsk:
			return approvalLineagePosture{capability: approvalAutoBaseline, valid: true}
		case claudePermAccept:
			return approvalLineagePosture{capability: approvalAutoSandbox, valid: true}
		case claudePermAuto:
			return approvalLineagePosture{capability: approvalAutoClassifier, valid: true}
		case claudePermBypass:
			return approvalLineagePosture{capability: approvalAutoAll, valid: true}
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
				capability |= approvalAutoSandbox
			}
			// `never` produces no approval requests, so enabling the reviewer
			// alongside it grants no classifier capability.
			if autoReview && policy != ApprovalNever {
				capability |= approvalAutoClassifier
			}
			return approvalLineagePosture{capability: capability, valid: true}
		default:
			return approvalLineagePosture{}
		}
	default:
		return approvalLineagePosture{}
	}
}
