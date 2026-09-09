package claude

import "github.com/tofutools/tclaude/internal/backend/model"

func claudeApprovalModes() []model.ApprovalMode {
	return []model.ApprovalMode{model.ApprovalSupervised, model.ApprovalAutomatic, model.ApprovalInherit, model.ApprovalDefault, model.ApprovalManual, model.ApprovalPlan, model.ApprovalAcceptEdits, model.ApprovalAuto, model.ApprovalDontAsk, model.ApprovalBypassPermissions}
}

func nativeApprovalMode(mode model.ApprovalMode) string {
	switch mode {
	case model.ApprovalInherit:
		return ""
	case model.ApprovalSupervised, model.ApprovalDefault, model.ApprovalManual:
		return "manual"
	case model.ApprovalAutomatic, model.ApprovalAuto:
		return "auto"
	default:
		return string(mode)
	}
}

func claudeApprovalDescriptions() map[model.ApprovalMode]string {
	return map[model.ApprovalMode]string{
		model.ApprovalInherit:           "Use the operator's native permission settings without a permission-mode override. This may wait for an operator.",
		model.ApprovalDefault:           "Standard interactive permissions, named manual by current Claude Code. This may wait for an operator.",
		model.ApprovalManual:            "Standard interactive permissions. This may wait for an operator.",
		model.ApprovalPlan:              "Read-only planning through the native permission mode.",
		model.ApprovalAcceptEdits:       "Approve native file edits automatically; other actions may ask the operator.",
		model.ApprovalAuto:              "Use Claude Code's automatic approval supervisor.",
		model.ApprovalDontAsk:           "Deny actions that are not pre-approved instead of asking the operator.",
		model.ApprovalBypassPermissions: "Bypass native permission prompts. The separately selected confinement still applies.",
	}
}
