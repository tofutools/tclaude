package sqlite

import (
	"context"
	"reflect"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func ownerSpawnLineageMatches(ctx context.Context, q queryer, assignment model.RoleAssignment, request model.AuthorityRequest) bool {
	proof := request.SpawnLineage
	if proof == nil || assignment.RoleID != model.GroupOwnerRole || request.RequestedConfiguration == nil || request.RequestedEnvironment != nil || request.RequestedHostSandbox != nil {
		return false
	}
	b := assignment.Bounds
	if b.AutoReview || len(b.Harnesses)+len(b.Models)+len(b.WorkingDirectoryRoots)+len(b.ApprovalModes)+len(b.SandboxModes)+len(b.HostSandboxProfiles)+len(b.Environments) != 0 {
		return false
	}
	if request.Principal.Kind != model.PrincipalAgent && request.Principal.Kind != model.PrincipalExecution || request.Principal.AgentID != proof.ParentAgentID {
		return false
	}
	if assignment.Resource.GroupID != proof.GroupID || assignment.Resource.Kind != model.ResourceGroup && assignment.Resource.Kind != model.ResourceGroupPeers {
		return false
	}
	switch request.Action {
	case model.ActionCreateGroupMember:
		if request.Resource.Kind != model.ResourceGroup || request.Resource.GroupID != proof.GroupID || !request.RequestedConfiguration.Equal(proof.Authored) {
			return false
		}
	case model.ActionLaunch:
		if !request.RequestedConfiguration.Equal(proof.Resolved) {
			return false
		}
	default:
		return false
	}
	parent, err := scanAgent(q.QueryRowContext(ctx, agentSelect+` WHERE id=?`, proof.ParentAgentID))
	if err != nil || parent.Lifecycle != model.AgentActive || parent.PrimaryExecutionID != proof.ParentExecutionID {
		return false
	}
	if request.Principal.ExecutionID != "" && request.Principal.ExecutionID != proof.ParentExecutionID {
		return false
	}
	var member int
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id=? AND agent_id=?`, proof.GroupID, proof.ParentAgentID).Scan(&member); err != nil || member != 1 {
		return false
	}
	execution, err := scanExecution(q.QueryRowContext(ctx, executionSelect+` WHERE id=?`, proof.ParentExecutionID))
	return err == nil && execution.AgentID == proof.ParentAgentID && (execution.State == model.ExecutionRunning || execution.State == model.ExecutionReleased) && reflect.DeepEqual(execution.Spec, proof.ParentSpec)
}
