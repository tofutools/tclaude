package app

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) AuthenticateAction(ctx context.Context, bearer []byte) (model.Principal, error) {
	if len(bearer) < 32 {
		return model.Principal{}, fail(ErrUnauthorized, "invalid execution credential")
	}
	digest := sha256.Sum256(bearer)
	access, err := s.store.AuthenticateExecutionAccess(ctx, digest[:], s.now().UTC())
	if err != nil {
		return model.Principal{}, fail(ErrUnauthorized, "invalid execution credential")
	}
	return model.ExecutionPrincipal(access.ExecutionID, access.AgentID, access.Generation), nil
}

func (s *Service) WhoAmI(ctx context.Context, req WhoAmIRequest) (WhoAmIResult, error) {
	if req.Principal.Kind != model.PrincipalExecution {
		return WhoAmIResult{}, fail(ErrUnauthorized, "execution identity required")
	}
	now := s.now().UTC()
	resource := model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: req.Principal.ExecutionID}
	if req.Principal.AgentID != "" {
		resource = model.ResourceSelector{Kind: model.ResourceAgent, AgentID: req.Principal.AgentID}
	}
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadIdentity, Resource: resource}, now); err != nil {
		return WhoAmIResult{}, err
	}
	execution, err := s.store.Execution(ctx, req.Principal.ExecutionID)
	if err != nil {
		return WhoAmIResult{}, err
	}
	result := WhoAmIResult{Principal: req.Principal, Execution: execution, ContextReadiness: execution.ContextReadiness, EvaluatedAt: now}
	result.Execution.Evidence = model.ProviderEvidence{}
	if req.Principal.AgentID != "" {
		agent, err := s.store.Agent(ctx, req.Principal.AgentID)
		if err != nil {
			return WhoAmIResult{}, err
		}
		result.Agent = &agent
		if association, err := s.store.CurrentConversation(ctx, agent.ID); err == nil {
			result.CurrentConversation = &association
		}
	}
	for _, action := range allActions {
		decision, err := s.store.Authorize(ctx, model.AuthorityRequest{Principal: req.Principal, Action: action, Resource: resource}, now)
		if err == nil && decision.Allowed {
			result.EffectiveActions = append(result.EffectiveActions, action)
		}
	}
	return result, nil
}

func (s *Service) ReadInbox(ctx context.Context, req ReadInboxRequest) (InboxResult, error) {
	if req.Principal.AgentID == "" {
		return InboxResult{}, fail(ErrUnsupported, "standalone execution has no agent inbox")
	}
	resource := model.ResourceSelector{Kind: model.ResourceAgent, AgentID: req.Principal.AgentID}
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadInbox, Resource: resource}, s.now().UTC()); err != nil {
		return InboxResult{}, err
	}
	messages, err := s.store.MessagesForAgent(ctx, req.Principal.AgentID, req.UnreadOnly)
	for index := range messages {
		messages[index] = messageForRecipient(messages[index], req.Principal.AgentID)
	}
	return InboxResult{Messages: messages}, err
}

func messageForRecipient(message model.Message, agentID model.AgentID) model.Message {
	recipients := make([]model.MessageRecipient, 0, len(message.Recipients))
	for _, recipient := range message.Recipients {
		if recipient.AddressKind != model.MessageAddressAgent || recipient.AgentID != agentID {
			recipient = model.MessageRecipient{AddressKind: recipient.AddressKind, AgentID: recipient.AgentID, Audience: recipient.Audience}
		}
		recipients = append(recipients, recipient)
	}
	message.Recipients = recipients
	return message
}

func (s *Service) ReadStatus(ctx context.Context, req ReadStatusRequest) (StatusResult, error) {
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadStatus, Resource: req.Target}, s.now().UTC()); err != nil {
		return StatusResult{}, err
	}
	snapshot, err := s.store.Snapshot(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	var result StatusResult
	groupMembers := map[model.AgentID]bool{}
	visibleAgents := map[model.AgentID]bool{}
	if req.Target.Kind == model.ResourceGroup || req.Target.Kind == model.ResourceGroupPeers {
		for _, group := range snapshot.Groups {
			if group.ID == req.Target.GroupID {
				for _, member := range group.Members {
					groupMembers[member] = true
				}
			}
		}
	}
	for _, agent := range snapshot.Agents {
		if req.Target.Kind == model.ResourceAgent && agent.ID == req.Target.AgentID || req.Target.Kind == model.ResourceSelf && agent.ID == req.Principal.AgentID || groupMembers[agent.ID] {
			result.Agents = append(result.Agents, agent)
			visibleAgents[agent.ID] = true
		}
	}
	for _, execution := range snapshot.Executions {
		visible := req.Target.Kind == model.ResourceExecution && execution.ID == req.Target.ExecutionID
		visible = visible || req.Target.Kind == model.ResourceAgent && execution.AgentID == req.Target.AgentID
		visible = visible || req.Target.Kind == model.ResourceSelf && execution.ID == req.Principal.ExecutionID
		visible = visible || groupMembers[execution.AgentID]
		if visible {
			execution.Evidence = model.ProviderEvidence{}
			result.Executions = append(result.Executions, execution)
			if execution.AgentID != "" {
				visibleAgents[execution.AgentID] = true
			}
		}
	}
	for _, association := range snapshot.Associations {
		if association.Current && visibleAgents[association.AgentID] {
			result.Associations = append(result.Associations, association)
		}
	}
	return result, nil
}

func (s *Service) ExplainAuthority(ctx context.Context, req AuthorityExplanationRequest) (AuthorityExplanationResult, error) {
	decision, err := s.store.Authorize(ctx, model.AuthorityRequest{Principal: req.Principal, Action: req.Action, Resource: req.Resource, RequestedConfiguration: req.RequestedConfiguration}, s.now().UTC())
	return AuthorityExplanationResult{Decision: decision}, err
}

func (s *Service) ListAuthority(ctx context.Context, req ListAuthorityRequest) (AuthorityStateResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return AuthorityStateResult{}, err
	}
	return s.store.AuthorityState(ctx)
}

func (s *Service) PutGrant(ctx context.Context, req PutGrantRequest) (GrantResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return GrantResult{}, err
	}
	if err := validateGrant(req.Grant); err != nil {
		return GrantResult{}, err
	}
	now := s.now().UTC()
	grant := req.Grant
	if req.ExpectedRevision == 0 {
		grant.Revision, grant.CreatedAt = 1, now
	}
	grant.UpdatedAt = now
	stored, err := s.store.PutGrant(ctx, grant, req.ExpectedRevision)
	return GrantResult{Grant: stored}, err
}

func (s *Service) DeleteGrant(ctx context.Context, req DeleteGrantRequest) error {
	if err := requireOperator(req.Principal); err != nil {
		return err
	}
	return s.store.DeleteGrant(ctx, req.GrantID, req.ExpectedRevision)
}

func (s *Service) PutRole(ctx context.Context, req PutRoleRequest) (RoleResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return RoleResult{}, err
	}
	if err := req.Role.ID.Validate(); err != nil || strings.TrimSpace(req.Role.Name) == "" || len(req.Role.Actions) == 0 {
		return RoleResult{}, fail(ErrInvalid, "valid role id, name and actions are required")
	}
	now := s.now().UTC()
	role := req.Role
	if req.ExpectedRevision == 0 {
		role.Revision, role.CreatedAt = 1, now
	}
	role.UpdatedAt = now
	stored, err := s.store.PutRole(ctx, role, req.ExpectedRevision)
	return RoleResult{Role: stored}, err
}

func (s *Service) PutRoleAssignment(ctx context.Context, req PutRoleAssignmentRequest) (RoleAssignmentResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return RoleAssignmentResult{}, err
	}
	now := s.now().UTC()
	assignment := req.Assignment
	if req.ExpectedRevision == 0 {
		assignment.Revision, assignment.CreatedAt = 1, now
	}
	assignment.UpdatedAt = now
	stored, err := s.store.PutRoleAssignment(ctx, assignment, req.ExpectedRevision)
	return RoleAssignmentResult{Assignment: stored}, err
}

func (s *Service) DeleteRoleAssignment(ctx context.Context, req DeleteRoleAssignmentRequest) error {
	if err := requireOperator(req.Principal); err != nil {
		return err
	}
	return s.store.DeleteRoleAssignment(ctx, req.Assignment, req.ExpectedRevision)
}

func (s *Service) SetGroupOwner(ctx context.Context, req SetGroupOwnerRequest) (GroupResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return GroupResult{}, err
	}
	group, err := s.store.SetGroupOwner(ctx, req.GroupID, req.OwnerAgentID, req.Bounds, req.ExpectedGroupRevision, s.now().UTC())
	return GroupResult{Group: group}, err
}

func (s *Service) ExecutionAccessStatus(ctx context.Context, req ExecutionAccessStatusRequest) (ExecutionAccessStatusResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	access, err := s.store.ExecutionAccess(ctx, req.ExecutionID)
	if err == nil && access.State == model.ExecutionAccessActive && !s.now().UTC().Before(access.ExpiresAt) {
		access.State = model.ExecutionAccessExpired
	}
	return ExecutionAccessStatusResult{Access: accessBinding(access)}, err
}

func (s *Service) RevokeExecutionAccess(ctx context.Context, req RevokeExecutionAccessRequest) (ExecutionAccessStatusResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	existing, err := s.store.ExecutionAccess(ctx, req.ExecutionID)
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	var delivery ports.ActionCredentialDelivery
	if execution, executionErr := s.store.Execution(ctx, req.ExecutionID); executionErr == nil {
		if provider, ok := s.providers.Provider(execution.Spec.Harness); ok {
			if credentialProvider, ok := provider.(ports.ActionCredentialProvider); ok {
				delivery = credentialProvider.ActionCredentials()
			}
		}
	}
	access, err := s.store.RevokeExecutionAccess(ctx, req.ExecutionID, req.ExpectedRevision, s.now().UTC())
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	if delivery != nil {
		proof, inspectErr := delivery.InspectActionCredential(ctx, accessBinding(existing))
		if inspectErr != nil {
			return ExecutionAccessStatusResult{Access: accessBinding(access)}, inspectErr
		}
		if err := validateAccessProof(existing, proof); err != nil {
			return ExecutionAccessStatusResult{Access: accessBinding(access)}, err
		}
		receipt := ports.ActionCredentialReceipt{ExecutionID: proof.ExecutionID, Generation: proof.Generation, DeliveryID: proof.DeliveryID, Resource: proof.Resource, FileIdentity: proof.FileIdentity}
		if cleanupErr := delivery.RemoveActionCredential(ctx, receipt); cleanupErr != nil {
			return ExecutionAccessStatusResult{Access: accessBinding(access)}, cleanupErr
		}
	}
	return ExecutionAccessStatusResult{Access: accessBinding(access)}, nil
}

func (s *Service) RenewExecutionAccess(ctx context.Context, req RenewExecutionAccessRequest) (ExecutionAccessStatusResult, error) {
	execution, err := s.store.Execution(ctx, req.ExecutionID)
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	access, err := s.store.ExecutionAccess(ctx, req.ExecutionID)
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	now := s.now().UTC()
	if access.Revision != req.ExpectedRevision || access.State != model.ExecutionAccessActive || !now.Before(access.ExpiresAt) {
		return ExecutionAccessStatusResult{}, ErrConflict
	}
	provider, ok := s.providers.Provider(execution.Spec.Harness)
	if !ok {
		return ExecutionAccessStatusResult{}, fail(ErrUnavailable, "harness %q has no provider", execution.Spec.Harness)
	}
	credentialProvider, ok := provider.(ports.ActionCredentialProvider)
	if !ok {
		return ExecutionAccessStatusResult{}, fail(ErrUnsupported, "provider does not support execution credentials")
	}
	proof, err := credentialProvider.ActionCredentials().InspectActionCredential(ctx, accessBinding(access))
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	if err := validateAccessProof(access, proof); err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	secret, err := generateActionCredential()
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	defer clear(secret)
	material := ports.ActionCredentialMaterial{ExecutionID: execution.ID, Generation: access.Generation + 1, DeliveryID: access.DeliveryID, Secret: secret, ExpiresAt: now.Add(s.accessLease)}
	current := ports.ActionCredentialReceipt{ExecutionID: proof.ExecutionID, Generation: proof.Generation, DeliveryID: proof.DeliveryID, Resource: proof.Resource, FileIdentity: proof.FileIdentity}
	receipt, err := credentialProvider.ActionCredentials().RotateActionCredential(ctx, current, material)
	if err != nil {
		return ExecutionAccessStatusResult{}, err
	}
	if receipt.ExecutionID != execution.ID || receipt.Generation != material.Generation || receipt.DeliveryID != access.DeliveryID || receipt.FileIdentity == "" {
		return ExecutionAccessStatusResult{}, fail(ErrInvalid, "provider returned mismatched credential rotation receipt")
	}
	digest := sha256.Sum256(secret)
	settlementCtx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()
	settledAt := s.now().UTC()
	rotated, err := s.store.RotateExecutionAccess(settlementCtx, execution.ID, access.Generation, access.Revision, digest[:], receipt, now, material.ExpiresAt, settledAt)
	return ExecutionAccessStatusResult{Access: accessBinding(rotated)}, err
}

func (s *Service) SweepExecutionAccess(ctx context.Context) ExecutionAccessRenewalReport {
	now := s.now().UTC()
	accesses, err := s.store.ExecutionAccessesDue(ctx, now, now.Add(s.accessLease/3))
	if err != nil {
		return ExecutionAccessRenewalReport{Failed: []ExecutionAccessRenewalFailure{{Code: Code(err), Detail: err.Error()}}}
	}
	var report ExecutionAccessRenewalReport
	for _, access := range accesses {
		result, err := s.RenewExecutionAccess(ctx, RenewExecutionAccessRequest{ExecutionID: access.ExecutionID, ExpectedRevision: access.Revision})
		if err != nil {
			report.Failed = append(report.Failed, ExecutionAccessRenewalFailure{ExecutionID: access.ExecutionID, Code: Code(err), Detail: err.Error()})
			continue
		}
		report.Renewed = append(report.Renewed, result.Access)
	}
	return report
}

func validateAccessProof(access model.ExecutionAccess, proof ports.ActionCredentialRecoveryProof) error {
	if proof.ExecutionID != access.ExecutionID || proof.Generation != access.Generation || proof.DeliveryID != access.DeliveryID || proof.Resource == "" || proof.FileIdentity == "" {
		return fail(ErrInvalid, "host returned mismatched execution credential proof")
	}
	return nil
}

func (s *Service) requireAuthority(ctx context.Context, request model.AuthorityRequest, at time.Time) error {
	decision, err := s.store.Authorize(ctx, request, at)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fail(ErrUnauthorized, "%s is not allowed for requested resource", request.Action)
	}
	return nil
}

func accessBinding(access model.ExecutionAccess) model.ExecutionAccessBinding {
	return model.ExecutionAccessBinding{ExecutionID: access.ExecutionID, Generation: access.Generation, DeliveryID: access.DeliveryID, State: access.State, ExpiresAt: access.ExpiresAt, Revision: access.Revision}
}

func validateGrant(grant model.AuthorityGrant) error {
	if err := grant.ID.Validate(); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if grant.Subject.Kind != model.AuthorityAgent && grant.Subject.Kind != model.AuthorityExecution {
		return fail(ErrInvalid, "invalid authority subject")
	}
	if grant.Action == "" || grant.Resource.Kind == "" {
		return fail(ErrInvalid, "grant action and resource are required")
	}
	return nil
}

var allActions = []model.Action{
	model.ActionReadIdentity, model.ActionReadStatus, model.ActionReadInbox, model.ActionMarkInboxRead,
	model.ActionSendMessage, model.ActionLaunch, model.ActionInteract, model.ActionAttach, model.ActionStop,
	model.ActionChangeContext, model.ActionUpdateConfiguration, model.ActionRetireAgent, model.ActionReactivateAgent,
	model.ActionManageMembership, model.ActionReadAttachment,
	model.ActionReadHistory, model.ActionRefreshHistory, model.ActionSetHistoryMetadata, model.ActionRegisterWorkspace,
	model.ActionReadUsage, model.ActionRefreshUsage, model.ActionReadActivity,
	model.ActionCreateWorkspace, model.ActionInspectWorkspace, model.ActionRemoveWorkspace,
	model.ActionRestoreWorkspace,
	model.ActionStartWork, model.ActionRecordWorkEvidence, model.ActionDecideWork, model.ActionCancelWork, model.ActionResolveWork,
	model.ActionStartShell,
}
