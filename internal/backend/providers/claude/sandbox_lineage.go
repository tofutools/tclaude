package claude

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/lineage"
)

func (*Provider) RequestedSandboxPosture(spec model.ResolvedExecutionSpec) model.SandboxPosture {
	if spec.Harness != Name || !lineage.Resolved(spec) {
		return model.SandboxPostureUnknown
	}
	if spec.Sandbox == model.SandboxWorkspaceWrite {
		return model.SandboxPostureClaudeConfined
	}
	return model.SandboxPostureUnknown
}

func (p *Provider) RecordedSandboxPosture(execution model.Execution) model.SandboxPosture {
	recorded, err := decodeEvidence(execution.Evidence)
	if err != nil || !lineage.Matches(execution, recorded.ExecutionID, recorded.HostSandboxPolicyHash, recorded.HostSandbox) {
		return model.SandboxPostureUnknown
	}
	return p.RequestedSandboxPosture(execution.Spec)
}
