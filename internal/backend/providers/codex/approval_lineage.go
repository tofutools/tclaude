package codex

import "github.com/tofutools/tclaude/internal/backend/model"

func (*Provider) ApprovalPosture(d model.DesiredConfiguration) model.ApprovalPosture {
	if d.Harness != Name {
		return model.ApprovalPosture{}
	}
	p := model.ApprovalPosture{Known: true}
	switch d.Approval {
	case model.ApprovalUntrusted:
	case model.ApprovalSupervised, model.ApprovalAutomatic, model.ApprovalNever, model.ApprovalOnRequest, model.ApprovalOnFailure:
		// The compatibility supervised mode renders on-request, not untrusted.
		p.Minimum = model.AutomaticInSandbox
	default:
		return model.ApprovalPosture{}
	}
	if d.AutoReview && d.Approval != model.ApprovalNever && d.Approval != model.ApprovalAutomatic {
		p.Minimum |= model.AutomaticReviewer
	}
	p.Maximum = p.Minimum
	return p
}
