package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func normalizeTeamTarget(instantiation model.TeamInstantiation) (model.TeamDeploymentTarget, error) {
	target := instantiation.Target
	if target.Kind == "" {
		if instantiation.GroupID == "" {
			return target, fmt.Errorf("explicit team target is required")
		}
		target = model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: instantiation.GroupID}
	} else if instantiation.GroupID != "" && instantiation.GroupID != target.GroupID {
		return target, fmt.Errorf("legacy group id conflicts with explicit target")
	}
	if target.Kind != model.TeamTargetNewGroup && target.Kind != model.TeamTargetExistingGroup {
		return target, fmt.Errorf("team target kind is unsupported")
	}
	if err := target.GroupID.Validate(); err != nil {
		return target, err
	}
	return target, nil
}

func (s *Service) prepareTeamWorkspaces(ctx context.Context, req DeployTeamRequest, team model.TeamDefinition) (map[string]model.TeamWorkspaceBinding, []model.WorkspaceID, error) {
	inputs := make(map[string]model.TeamWorkspaceInput, len(team.Members))
	switch team.WorkspacePolicy {
	case model.WorkspacePolicyShared:
		if req.Instantiation.Workspaces.Shared == nil || len(req.Instantiation.Workspaces.Members) != 0 {
			return nil, nil, fail(ErrInvalid, "shared workspace policy requires exactly one shared workspace input")
		}
		for _, member := range team.Members {
			inputs[member.Key] = *req.Instantiation.Workspaces.Shared
		}
	case model.WorkspacePolicyPerMember:
		if req.Instantiation.Workspaces.Shared != nil || len(req.Instantiation.Workspaces.Members) != len(team.Members) {
			return nil, nil, fail(ErrInvalid, "per-member workspace policy requires one input for every member")
		}
		seen := make(map[model.WorkspaceID]bool, len(team.Members))
		for _, member := range team.Members {
			input, ok := req.Instantiation.Workspaces.Members[member.Key]
			if !ok {
				return nil, nil, fail(ErrInvalid, "workspace input for member %s is required", member.Key)
			}
			if seen[input.WorkspaceID] {
				return nil, nil, fail(ErrInvalid, "per-member workspace %s is selected more than once", input.WorkspaceID)
			}
			seen[input.WorkspaceID] = true
			inputs[member.Key] = input
		}
	default:
		return nil, nil, fail(ErrInvalid, "team workspace policy is unsupported")
	}
	bindings := make(map[string]model.TeamWorkspaceBinding, len(inputs))
	created := make(map[model.WorkspaceID]model.TeamWorkspaceBinding)
	var owned []model.WorkspaceID
	for _, member := range team.Members {
		input := inputs[member.Key]
		if binding, ok := created[input.WorkspaceID]; ok {
			bindings[member.Key] = binding
			continue
		}
		if err := input.WorkspaceID.Validate(); err != nil {
			return nil, nil, fail(ErrInvalid, "member %s workspace: %v", member.Key, err)
		}
		if (input.CreateIntent == nil) == (input.ExpectedRevision == 0) {
			return nil, nil, fail(ErrInvalid, "workspace %s requires exactly one create intent or expected revision", input.WorkspaceID)
		}
		var workspace model.Workspace
		ownedByDeployment := input.CreateIntent != nil
		if input.CreateIntent != nil {
			if strings.TrimSpace(input.CreateIntent.IntendedPath) == "" {
				return nil, nil, fail(ErrInvalid, "workspace %s requires an explicit intended path", input.WorkspaceID)
			}
			result, err := s.CreateCheckout(ctx, CreateCheckoutRequest{Context: RequestContext{Principal: req.Context.Principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(req.DeploymentID)+":workspace:"+string(input.WorkspaceID)))}, ID: input.WorkspaceID, Intent: *input.CreateIntent})
			if err != nil {
				return nil, nil, err
			}
			workspace, err = s.store.Workspace(ctx, result.Workspace.ID)
			if err != nil {
				return nil, nil, err
			}
			owned = append(owned, workspace.ID)
		} else {
			var err error
			workspace, err = s.store.Workspace(ctx, input.WorkspaceID)
			if err != nil {
				return nil, nil, err
			}
			if workspace.Revision != input.ExpectedRevision {
				return nil, nil, ErrConflict
			}
			if err = s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}, s.now().UTC()); err != nil {
				return nil, nil, err
			}
		}
		if workspace.State != model.WorkspaceAvailable || strings.TrimSpace(workspace.Observation.ActualPath) == "" {
			return nil, nil, fail(ErrConflict, "workspace %s is not available", workspace.ID)
		}
		binding := model.TeamWorkspaceBinding{WorkspaceID: workspace.ID, SelectedRevision: workspace.Revision, Owned: ownedByDeployment}
		created[workspace.ID] = binding
		bindings[member.Key] = binding
	}
	return bindings, owned, nil
}

func (s *Service) materializeDeploymentRhythms(ctx context.Context, deployment model.TeamDeployment, team model.TeamDefinition, request RequestContext) error {
	for index, source := range team.Automation {
		revision, err := s.store.AutomationRuleRevision(ctx, source.RevisionID)
		if err != nil {
			return err
		}
		if revision.RuleID != source.RuleID || revision.ContentHash != source.ContentHash {
			return fail(ErrConflict, "automation rule %s is not the pinned revision", source.RuleID)
		}
		if revision.Policy.Overlap == model.OverlapReplace {
			return fail(ErrInvalid, "deployment rhythm %s cannot use replace overlap", source.RuleID)
		}
		id := deployment.OwnedAutomationRuleIDs[index]
		revisionID := model.AutomationRuleRevisionID(deterministicOrchestrationID("rule_revision_", string(deployment.ID)+":"+string(source.RuleID)))
		_, err = s.saveAutomationRule(ctx, SaveAutomationRuleRequest{
			Context: RequestContext{Principal: request.Principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(deployment.ID)+":rhythm:"+string(source.RuleID)))},
			ID:      id, RevisionID: revisionID, Name: "deployment " + string(deployment.ID) + ": " + string(source.RuleID), Enabled: false,
			Owner: revision.Owner, Delegation: revision.Delegation, Condition: revision.Condition, Action: revision.Action,
			Policy: revision.Policy, Dependencies: revision.Dependencies,
		}, deployment.ID)
		if err != nil {
			return err
		}
	}
	return s.materializeAuthoredTeamRhythms(ctx, deployment, team.Rhythms, request)
}
