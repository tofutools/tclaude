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
	if assignment.Resource.GroupID != proof.GroupID || assignment.Resource.Kind != model.ResourceGroup && assignment.Resource.Kind != model.ResourceGroupPeers {
		return false
	}
	var member int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id=? AND agent_id=?`, proof.GroupID, proof.ParentAgentID).Scan(&member); err != nil || member != 1 {
		return false
	}
	return currentSpawnLineage(ctx, q, request)
}

func currentSpawnLineage(ctx context.Context, q queryer, request model.AuthorityRequest) bool {
	proof := request.SpawnLineage
	if proof == nil || request.RequestedConfiguration == nil || request.RequestedEnvironment != nil || request.RequestedHostSandbox != nil {
		return false
	}
	if request.Principal.Kind != model.PrincipalAgent && request.Principal.Kind != model.PrincipalExecution || request.Principal.AgentID != proof.ParentAgentID {
		return false
	}
	switch request.Action {
	case model.ActionCreateGroupMember:
		if request.Resource.Kind != model.ResourceGroup || request.Resource.GroupID != proof.GroupID || !request.RequestedConfiguration.Equal(proof.Authored) {
			return false
		}
	case model.ActionLaunch:
		if proof.ChildAgentID != "" && (request.Resource.Kind != model.ResourceAgent || request.Resource.AgentID != proof.ChildAgentID) {
			return false
		}
		if !request.RequestedConfiguration.Equal(proof.Resolved) {
			return false
		}
	case model.ActionSpawnGroupMember:
		if !proof.Atomic || request.Resource.Kind != model.ResourceGroup || request.Resource.GroupID != proof.GroupID || (!request.RequestedConfiguration.Equal(proof.Authored) && !request.RequestedConfiguration.Equal(proof.Resolved)) {
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
	execution, err := scanExecution(q.QueryRowContext(ctx, executionSelect+` WHERE id=?`, proof.ParentExecutionID))
	return err == nil && execution.AgentID == proof.ParentAgentID && (execution.State == model.ExecutionRunning || execution.State == model.ExecutionReleased) && reflect.DeepEqual(execution.Spec, proof.ParentSpec)
}

// An atomic spawn grant contributes both creation and its first launch, never
// authority to restart an existing agent. Provider lineage bounds the policy;
// an explicitly bounded grant still uses the ordinary allow-list.
func atomicSpawnBoundsMatch(ctx context.Context, q queryer, bounds model.ConfigurationBounds, request model.AuthorityRequest) bool {
	if request.Action != model.ActionSpawnGroupMember || bounds.AutoReview || len(bounds.Harnesses)+len(bounds.Models)+len(bounds.WorkingDirectoryRoots)+len(bounds.ApprovalModes)+len(bounds.SandboxModes)+len(bounds.HostSandboxProfiles)+len(bounds.Environments) != 0 {
		return false
	}
	return currentSpawnLineage(ctx, q, request)
}
