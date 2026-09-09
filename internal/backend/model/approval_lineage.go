package model

import "slices"

// AutomaticApproval separates powers that do not form a total ordering. Native
// providers classify their policies; the authority layer compares those claims.
// These powers describe approval, not filesystem or network confinement.
type AutomaticApproval uint8

const (
	AutomaticEdits AutomaticApproval = 1 << iota
	AutomaticCommands
	AutomaticReviewer
	AutomaticUnreviewed
	AutomaticInSandbox = AutomaticEdits | AutomaticCommands
	AutomaticAll       = AutomaticInSandbox | AutomaticReviewer | AutomaticUnreviewed
)

// ApprovalPosture carries a proven parent floor and a possible child ceiling.
// Unknown native settings must not become a positive delegation capability.
// Policy/continuation keys and exceptions are provider-owned internal evidence,
// never caller-supplied grants or public launch inputs.
type ApprovalPosture struct {
	Known           bool
	Minimum         AutomaticApproval
	Maximum         AutomaticApproval
	PolicyKey       string
	ContinuationKey string
	DelegatesTo     []string
}

func (p ApprovalPosture) Allows(child ApprovalPosture) bool {
	if !p.valid() || !child.valid() {
		return false
	}
	if p.ContinuationKey != "" && p.ContinuationKey == child.ContinuationKey {
		return true
	}
	if child.PolicyKey != "" && slices.Contains(p.DelegatesTo, child.PolicyKey) {
		return true
	}
	return child.Maximum&^p.Minimum == 0
}

func (p ApprovalPosture) valid() bool {
	return p.Known && p.Minimum&^AutomaticAll == 0 && p.Maximum&^AutomaticAll == 0 && p.Minimum&^p.Maximum == 0
}
