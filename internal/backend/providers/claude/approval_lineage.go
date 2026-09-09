package claude

import "github.com/tofutools/tclaude/internal/backend/model"

func (*Provider) ApprovalPosture(d model.DesiredConfiguration) model.ApprovalPosture {
	if d.Harness != Name || d.AutoReview {
		return model.ApprovalPosture{}
	}
	p := model.ApprovalPosture{Known: true}
	switch d.Approval {
	case model.ApprovalSupervised, model.ApprovalDefault, model.ApprovalManual, model.ApprovalPlan, model.ApprovalDontAsk:
	case model.ApprovalAcceptEdits:
		p.Minimum = model.AutomaticEdits
	case model.ApprovalAutomatic, model.ApprovalAuto:
		p.Minimum = model.AutomaticInSandbox
		// V1's explicit compatibility exception. Sandbox lineage remains separate.
		p.DelegatesTo = []string{"copilot:yolo"}
	case model.ApprovalBypassPermissions:
		p.Minimum = model.AutomaticAll
	case model.ApprovalInherit:
		p.Maximum = model.AutomaticInSandbox | model.AutomaticReviewer
		p.ContinuationKey = "claude:inherit"
		return p
	default:
		return model.ApprovalPosture{}
	}
	p.Maximum = p.Minimum
	return p
}
