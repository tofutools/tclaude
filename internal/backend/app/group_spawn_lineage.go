package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) groupSpawnLineage(ctx context.Context, in CreateGroupMemberRequest, child model.Agent) (*model.SpawnLineage, error) {
	request := model.AuthorityRequest{Principal: in.Context.Principal, Action: model.ActionCreateGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: in.GroupID}, RequestedConfiguration: &child.Desired}
	decision, err := s.store.Authorize(ctx, request, s.now().UTC())
	if err != nil {
		return nil, err
	}
	if decision.Allowed {
		return nil, nil
	}
	if decision.SourceKind == model.AuthorityDenied {
		return nil, ErrUnauthorized
	}
	principal := in.Context.Principal
	if principal.AgentID == "" || principal.Kind != model.PrincipalAgent && principal.Kind != model.PrincipalExecution {
		return nil, ErrUnauthorized
	}
	parent, err := s.store.Agent(ctx, principal.AgentID)
	if err != nil {
		return nil, err
	}
	if parent.Lifecycle != model.AgentActive || parent.PrimaryExecutionID == "" || principal.ExecutionID != "" && principal.ExecutionID != parent.PrimaryExecutionID {
		return nil, ErrUnauthorized
	}
	execution, err := s.store.Execution(ctx, parent.PrimaryExecutionID)
	if err != nil {
		return nil, err
	}
	if execution.AgentID != parent.ID || execution.State != model.ExecutionRunning && execution.State != model.ExecutionReleased {
		return nil, fail(ErrUnauthorized, "owner needs a current running execution before spawning members")
	}
	parentProvider, ok := s.providers.Provider(execution.Spec.Harness)
	if !ok {
		return nil, ErrUnavailable
	}
	childProvider, ok := s.providers.Provider(child.Desired.Harness)
	if !ok {
		return nil, ErrUnavailable
	}
	parentApproval, ok := parentProvider.(ports.ApprovalLineageProvider)
	if !ok {
		return nil, ErrUnsupported
	}
	childApproval, ok := childProvider.(ports.ApprovalLineageProvider)
	if !ok {
		return nil, ErrUnsupported
	}
	parentSandbox, ok := parentProvider.(ports.SandboxLineageProvider)
	if !ok {
		return nil, ErrUnsupported
	}
	childSandbox, ok := childProvider.(ports.SandboxLineageProvider)
	if !ok {
		return nil, ErrUnsupported
	}
	parentDesired := model.DesiredConfiguration{Harness: execution.Spec.Harness, Approval: execution.Spec.Approval, AutoReview: execution.Spec.AutoReview}
	if !parentApproval.ApprovalPosture(parentDesired).Allows(childApproval.ApprovalPosture(child.Desired)) {
		return nil, fail(ErrUnauthorized, "child approval is broader than the owner's running approval mode")
	}
	desired := child.Desired
	desired.HostSandbox, err = s.launchSandboxSelection(ctx, desired.HostSandbox, child)
	if err != nil {
		return nil, err
	}
	preparedDesired := desired
	materialized, err := s.prepareProviderSandbox(ctx, childProvider, desired.HostSandbox)
	if err != nil {
		return nil, err
	}
	if materialized != nil {
		selection, selectErr := materialized.LaunchSelection()
		if selectErr != nil {
			return nil, selectErr
		}
		selection.GroupID = desired.HostSandbox.GroupID
		preparedDesired.HostSandbox = &selection
	}
	spec := resolvedSpec("", child.ID, preparedDesired, "")
	if !parentSandbox.RecordedSandboxPosture(execution).Allows(childSandbox.RequestedSandboxPosture(spec)) {
		return nil, fail(ErrUnauthorized, "child confinement is broader than the owner's running sandbox")
	}
	return &model.SpawnLineage{GroupID: in.GroupID, ParentAgentID: parent.ID, ParentExecutionID: execution.ID, ParentSpec: execution.Spec, Authored: child.Desired, Resolved: desired, PreparedSandbox: model.CloneSandboxSelection(preparedDesired.HostSandbox)}, nil
}
