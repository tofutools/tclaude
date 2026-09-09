package opencode

import "github.com/tofutools/tclaude/internal/backend/model"

func (*Provider) ApprovalPosture(d model.DesiredConfiguration) model.ApprovalPosture {
	if d.Harness != Name || d.AutoReview {
		return model.ApprovalPosture{}
	}
	p := model.ApprovalPosture{Known: true}
	switch d.Approval {
	case model.ApprovalDeny, model.ApprovalAsk, model.ApprovalSupervised:
	case model.ApprovalAllowTools:
		p.Minimum = model.AutomaticEdits
	case model.ApprovalAutomatic:
		// Legacy v2 automatic allows bash as well as edit; it is broader than the
		// native allow-tools mode and must never be classified as edits alone.
		p.Minimum = model.AutomaticInSandbox
	default:
		return model.ApprovalPosture{}
	}
	p.Maximum = p.Minimum
	return p
}
