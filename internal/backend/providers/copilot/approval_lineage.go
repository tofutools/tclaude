package copilot

import "github.com/tofutools/tclaude/internal/backend/model"

func (*Provider) ApprovalPosture(d model.DesiredConfiguration) model.ApprovalPosture {
	if d.Harness != Name || d.AutoReview {
		return model.ApprovalPosture{}
	}
	p := model.ApprovalPosture{Known: true}
	switch d.Approval {
	case model.ApprovalAutomatic, model.ApprovalAllowTools:
		p.Minimum = model.AutomaticInSandbox
	case model.ApprovalYolo:
		p.Minimum = model.AutomaticAll
		p.PolicyKey = "copilot:yolo"
	case model.ApprovalInherit, model.ApprovalSupervised:
		// Both omit native approval flags; remembered operator choices are unknown.
		p.Maximum = model.AutomaticAll
		return p
	default:
		return model.ApprovalPosture{}
	}
	p.Maximum = p.Minimum
	return p
}
