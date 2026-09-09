package opencode

import "github.com/tofutools/tclaude/internal/backend/model"

func toolPermissionRules(approval model.ApprovalMode, sandbox model.SandboxMode, network model.SandboxNetworkBaseline, tools model.ToolGovernance) []permissionRule {
	rules := permissionRules(approval, sandbox, network)
	// Empty preserves existing generic modes and the native default baseline.
	if tools == "" {
		return rules
	}
	for _, permission := range []string{"bash", "glob", "grep", "lsp", "task", "skill"} {
		rules = append(rules, permissionRule{Permission: permission, Pattern: "*", Action: string(tools)})
	}
	return rules
}
