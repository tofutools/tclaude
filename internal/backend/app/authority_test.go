package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestExecutionAccessAuthorityMailRotationAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	provider := newAccessProvider()
	service := testAccessService(store, provider)
	operator := model.OperatorPrincipal()
	a := createAgent(t, ctx, service, operator, "agent_auth_a")
	b := createAgent(t, ctx, service, operator, "agent_auth_b")
	bounds := configurationBounds(a.Desired)
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "group_auth", Name: "Auth", Members: []model.AgentID{a.ID, b.ID}, OwnerAgentID: a.ID, OwnerBounds: bounds})
	require.NoError(t, err)

	launchedA, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_auth_a"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: a.ID, ExpectedRevision: a.Revision}}})
	require.NoError(t, err)
	firstSecret := append([]byte(nil), provider.delivery.secret...)
	require.Len(t, firstSecret, 64)
	require.Regexp(t, `^[0-9a-f]{64}$`, string(firstSecret))
	callerA, err := service.AuthenticateAction(ctx, firstSecret)
	require.NoError(t, err)
	require.Equal(t, launchedA.Execution.ID, callerA.ExecutionID)
	identity, err := service.WhoAmI(ctx, app.WhoAmIRequest{Principal: callerA})
	require.NoError(t, err)
	require.Equal(t, a.ID, identity.Agent.ID)
	require.Empty(t, identity.Execution.Evidence.Provider)
	groupStatus, err := service.ReadStatus(ctx, app.ReadStatusRequest{Principal: callerA, Target: model.ResourceSelector{Kind: model.ResourceGroupPeers, GroupID: "group_auth"}})
	require.NoError(t, err)
	require.Len(t, groupStatus.Agents, 2)
	require.Len(t, groupStatus.Executions, 1)
	require.Len(t, groupStatus.Associations, 1)
	require.Equal(t, a.ID, groupStatus.Associations[0].AgentID)
	require.True(t, groupStatus.Associations[0].Current)

	sent, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(callerA, "same_request"), RecipientAgentIDs: []model.AgentID{b.ID}, Body: "offline"})
	require.NoError(t, err)
	status, err := service.ExecutionAccessStatus(ctx, app.ExecutionAccessStatusRequest{Principal: operator, ExecutionID: launchedA.Execution.ID})
	require.NoError(t, err)
	require.NotEmpty(t, status.Access.DeliveryID, "application allocates delivery identity before provider preparation")
	renewed, err := service.RenewExecutionAccess(ctx, app.RenewExecutionAccessRequest{ExecutionID: launchedA.Execution.ID, ExpectedRevision: status.Access.Revision})
	require.NoError(t, err)
	require.Equal(t, model.AccessGeneration(2), renewed.Access.Generation)
	_, err = service.AuthenticateAction(ctx, firstSecret)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	callerA, err = service.AuthenticateAction(ctx, provider.delivery.secret)
	require.NoError(t, err)
	repeated, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(callerA, "same_request"), RecipientAgentIDs: []model.AgentID{b.ID}, Body: "offline"})
	require.NoError(t, err)
	require.Equal(t, sent.Message.ID, repeated.Message.ID)

	launchedB, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_auth_b"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: b.ID, ExpectedRevision: b.Revision}}})
	require.NoError(t, err)
	callerB, err := service.AuthenticateAction(ctx, provider.delivery.secrets[launchedB.Execution.ID])
	require.NoError(t, err)
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: callerB})
	require.NoError(t, err)
	require.Len(t, inbox.Messages, 1)
	_, err = service.MarkMessageRead(ctx, app.MarkMessageReadRequest{RequestContext: effect(callerB, "mark_b"), MessageID: inbox.Messages[0].ID, AgentID: a.ID})
	require.NoError(t, err, "body agent id cannot redirect the authenticated recipient")

	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	restarted := testAccessService(store, provider)
	_, err = restarted.AuthenticateAction(ctx, provider.delivery.secrets[launchedA.Execution.ID])
	require.ErrorIs(t, err, app.ErrUnauthorized, "startup suspension denies query-side resurrection")
	_, err = restarted.Recover(ctx, app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	_, err = restarted.AuthenticateAction(ctx, provider.delivery.secrets[launchedA.Execution.ID])
	require.NoError(t, err)
}

func TestLiveGrantRevocationBlocksEffectAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "grant.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newAccessProvider()
	service := testAccessService(store, provider)
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_grant")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_grant"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	caller, err := service.AuthenticateAction(ctx, provider.delivery.secrets[launched.Execution.ID])
	require.NoError(t, err)
	grantResult, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "grant_interact", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: agent.ID}, Action: model.ActionInteract, Resource: model.ResourceSelector{Kind: model.ResourceSelf}}})
	require.NoError(t, err)
	_, err = service.Interact(ctx, app.InteractRequest{RequestContext: effect(caller, "granted_interact"), ExecutionID: launched.Execution.ID, Text: "allowed"})
	require.NoError(t, err)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grantResult.Grant.ID, ExpectedRevision: grantResult.Grant.Revision}))
	_, err = service.Interact(ctx, app.InteractRequest{RequestContext: effect(caller, "revoked_interact"), ExecutionID: launched.Execution.ID, Text: "denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized)
}

func TestAutomationDelegationIntersectsLiveOwnerAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "automation.db"))
	require.NoError(t, err)
	defer store.Close()
	service := testAccessService(store, newAccessProvider())
	operator := model.OperatorPrincipal()
	owner := createAgent(t, ctx, service, operator, "agent_automation_owner")
	target := createAgent(t, ctx, service, operator, "agent_automation_target")
	outside := createAgent(t, ctx, service, operator, "agent_automation_outside")
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{
		ID:       "grant_automation_send",
		Subject:  model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: owner.ID},
		Action:   model.ActionSendMessage,
		Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target.ID},
	}})
	require.NoError(t, err)
	automation := model.AutomationPrincipal("run_daily_triage", model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: owner.ID}, model.AutomationDelegation{
		Actions:   []model.Action{model.ActionSendMessage},
		Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: target.ID}},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	sent, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "automation_send"), RecipientAgentIDs: []model.AgentID{target.ID}, Body: "delegated"})
	require.NoError(t, err)
	require.Equal(t, "run_daily_triage", sent.Message.Sender.AutomationRun)
	require.Equal(t, owner.ID, sent.Message.Sender.Authority.AgentID)

	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "automation_outside"), RecipientAgentIDs: []model.AgentID{outside.ID}, Body: "denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized, "the accepted run fixture cannot broaden its resource scope")
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "automation_revoked"), RecipientAgentIDs: []model.AgentID{target.ID}, Body: "denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized, "delegation remains bounded by the owner's live grants")
}

func TestAutomationNoExpiryStillIntersectsLiveOwnerAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "automation.db"))
	require.NoError(t, err)
	defer store.Close()
	service := testAccessService(store, newAccessProvider())
	operator := model.OperatorPrincipal()
	owner := createAgent(t, ctx, service, operator, "agent_automation_owner")
	target := createAgent(t, ctx, service, operator, "agent_automation_target")
	outside := createAgent(t, ctx, service, operator, "agent_automation_outside")
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{
		ID:       "grant_automation_send",
		Subject:  model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: owner.ID},
		Action:   model.ActionSendMessage,
		Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target.ID},
	}})
	require.NoError(t, err)
	automation := model.AutomationPrincipal("run_daily_triage", model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: owner.ID}, model.AutomationDelegation{
		Actions:   []model.Action{model.ActionSendMessage},
		Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: target.ID}},
		NoExpiry:  true,
	})
	automation.Delegation.NoExpiry = false
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "implicit_no_expiry"), RecipientAgentIDs: []model.AgentID{target.ID}, Body: "denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized, "an omitted expiry does not silently opt in")
	automation.Delegation.NoExpiry = true
	sent, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "automation_send"), RecipientAgentIDs: []model.AgentID{target.ID}, Body: "delegated"})
	require.NoError(t, err)
	require.Equal(t, "run_daily_triage", sent.Message.Sender.AutomationRun)
	require.Equal(t, owner.ID, sent.Message.Sender.Authority.AgentID)

	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "automation_outside"), RecipientAgentIDs: []model.AgentID{outside.ID}, Body: "denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized, "the accepted run fixture cannot broaden its resource scope")
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(automation, "automation_revoked"), RecipientAgentIDs: []model.AgentID{target.ID}, Body: "denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized, "delegation remains bounded by the owner's live grants")
}

func TestExecutionAccessSweepOwnsLeaseEligibility(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "sweep.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newAccessProvider()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	service := testAccessService(store, provider).WithClock(func() time.Time { return now })
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_sweep")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "launch_sweep"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	require.Empty(t, service.SweepExecutionAccess(ctx).Renewed, "fresh access is outside the application renewal window")
	now = now.Add(17 * time.Hour)
	report := service.SweepExecutionAccess(ctx)
	require.Empty(t, report.Failed)
	require.Len(t, report.Renewed, 1)
	require.Equal(t, launched.Execution.ID, report.Renewed[0].ExecutionID)
	require.Equal(t, model.AccessGeneration(2), report.Renewed[0].Generation)
	require.Empty(t, service.SweepExecutionAccess(ctx).Renewed, "a rotated lease is no longer due")
}

type inspectionFailureDelivery struct{ ports.ActionCredentialDelivery }

func (inspectionFailureDelivery) InspectActionCredential(context.Context, model.ExecutionAccessBinding) (ports.ActionCredentialRecoveryProof, error) {
	return ports.ActionCredentialRecoveryProof{}, errors.New("credential resource missing")
}

type inspectionFailureProvider struct{ *accessProvider }

func (p *inspectionFailureProvider) ActionCredentials() ports.ActionCredentialDelivery {
	return inspectionFailureDelivery{ActionCredentialDelivery: p.delivery}
}

func TestRevocationDeniesBearerBeforeCredentialCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "revoke.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := &inspectionFailureProvider{accessProvider: newAccessProvider()}
	service := testAccessService(store, provider)
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_revoke_missing_resource")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_revoke_missing"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	secret := append([]byte(nil), provider.delivery.secret...)
	status, err := service.ExecutionAccessStatus(ctx, app.ExecutionAccessStatusRequest{Principal: operator, ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	revoked, err := service.RevokeExecutionAccess(ctx, app.RevokeExecutionAccessRequest{Principal: operator, ExecutionID: launched.Execution.ID, ExpectedRevision: status.Access.Revision})
	require.Error(t, err, "missing host resource is reported as cleanup failure")
	require.Equal(t, model.ExecutionAccessRevoked, revoked.Access.State)
	_, err = service.AuthenticateAction(ctx, secret)
	require.ErrorIs(t, err, app.ErrUnauthorized, "durable denial does not depend on host cleanup")
}

type permissiveAuthorizeStore struct{ app.Store }

func (*permissiveAuthorizeStore) Authorize(_ context.Context, request model.AuthorityRequest, _ time.Time) (model.AuthorityDecision, error) {
	return model.AuthorityDecision{Allowed: true, Action: request.Action, Resource: request.Resource}, nil
}

func TestConfigurationAndReadMutationsAuthorizeInsideTransaction(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "atomic-authority.db"))
	require.NoError(t, err)
	defer store.Close()
	wrapped := &permissiveAuthorizeStore{Store: store}
	service := testAccessService(wrapped, newAccessProvider())
	operator := model.OperatorPrincipal()
	target := createAgent(t, ctx, service, operator, "agent_atomic_target")
	automation := model.AutomationPrincipal("run_atomic", model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: target.ID}, model.AutomationDelegation{
		Actions:   []model.Action{model.ActionUpdateConfiguration, model.ActionMarkInboxRead},
		Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: target.ID}},
		Bounds:    configurationBounds(target.Desired),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	_, err = service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: automation, ID: target.ID, ExpectedRevision: target.Revision, Name: "must not change", Desired: target.Desired})
	require.ErrorIs(t, err, app.ErrUnauthorized, "store transaction ignores a separately forged authorization result")

	message, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(operator, "atomic_message"), RecipientAgentIDs: []model.AgentID{target.ID}, Body: "unread"})
	require.NoError(t, err)
	_, err = service.MarkMessageRead(ctx, app.MarkMessageReadRequest{RequestContext: effect(automation, "atomic_read"), MessageID: message.Message.ID, AgentID: target.ID})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(target.ID), UnreadOnly: true})
	require.NoError(t, err)
	require.Len(t, inbox.Messages, 1, "denied transactional acknowledgement leaves the message unread")
}

func TestPrimaryContinuityMayRotateNativeReference(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "continuity.db"))
	require.NoError(t, err)
	defer store.Close()
	service := testAccessService(store, newAccessProvider())
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_continuity_rotation")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_continuity_rotation"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.NotNil(t, execution.NativeConversation)
	priorConversation := execution.ConversationID
	rotated, err := store.AdmitPrimaryContext(ctx, app.PrimaryContextAdmission{Evidence: ports.PrimaryContextEvidence{
		ExecutionID: execution.ID, Attempt: execution.Attempt, Provider: "fake", PrimaryCorrelation: "primary_continuity",
		Disposition:        ports.PrimaryContextContinuity,
		PriorBinding:       &model.NativeBinding{Namespace: execution.NativeConversation.Namespace, Reference: execution.NativeConversation.Reference},
		NextBinding:        &model.NativeBinding{Namespace: execution.NativeConversation.Namespace, Reference: "native_rotated_without_reset"},
		PriorProviderOrder: execution.ContextOrder, ProviderOrder: "continuity_rotation_2", ObservedAt: time.Now(),
	}, At: time.Now()})
	require.NoError(t, err)
	require.Equal(t, priorConversation, rotated.ConversationID)
	require.Equal(t, "native_rotated_without_reset", rotated.NativeConversation.Reference)
}

func TestExpiredAccessCannotBeRenewed(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "expired-renewal.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newAccessProvider()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	service := testAccessService(store, provider).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_expired_renewal")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_expired_renewal"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	status, err := service.ExecutionAccessStatus(ctx, app.ExecutionAccessStatusRequest{Principal: operator, ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	now = now.Add(25 * time.Hour)
	_, err = service.RenewExecutionAccess(ctx, app.RenewExecutionAccessRequest{ExecutionID: launched.Execution.ID, ExpectedRevision: status.Access.Revision})
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.RotateExecutionAccess(ctx, launched.Execution.ID, status.Access.Generation, status.Access.Revision, make([]byte, 32), ports.ActionCredentialReceipt{
		ExecutionID: launched.Execution.ID, Generation: status.Access.Generation + 1, DeliveryID: status.Access.DeliveryID, Resource: "/private/credential", FileIdentity: "expired-rotation",
	}, now, now.Add(24*time.Hour), now)
	require.ErrorIs(t, err, app.ErrConflict, "the durable CAS independently fences expiry at settlement time")
}

type cancelAfterRotateDelivery struct {
	ports.ActionCredentialDelivery
	cancel context.CancelFunc
}

func (d cancelAfterRotateDelivery) RotateActionCredential(ctx context.Context, current ports.ActionCredentialReceipt, material ports.ActionCredentialMaterial) (ports.ActionCredentialReceipt, error) {
	receipt, err := d.ActionCredentialDelivery.RotateActionCredential(ctx, current, material)
	if err == nil {
		d.cancel()
	}
	return receipt, err
}

type cancelAfterRotateProvider struct {
	*accessProvider
	cancel context.CancelFunc
}

func (p *cancelAfterRotateProvider) ActionCredentials() ports.ActionCredentialDelivery {
	return cancelAfterRotateDelivery{ActionCredentialDelivery: p.delivery, cancel: p.cancel}
}

func TestSuccessfulHostRotationSettlesAfterCallerCancellation(t *testing.T) {
	baseCtx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "rotation-settlement.db"))
	require.NoError(t, err)
	defer store.Close()
	ctx, cancel := context.WithCancel(baseCtx)
	provider := &cancelAfterRotateProvider{accessProvider: newAccessProvider(), cancel: cancel}
	service := testAccessService(store, provider)
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_rotation_settlement")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_rotation_settlement"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	status, err := service.ExecutionAccessStatus(ctx, app.ExecutionAccessStatusRequest{Principal: operator, ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	renewed, err := service.RenewExecutionAccess(ctx, app.RenewExecutionAccessRequest{ExecutionID: launched.Execution.ID, ExpectedRevision: status.Access.Revision})
	require.NoError(t, err)
	require.Equal(t, model.AccessGeneration(2), renewed.Access.Generation)
	_, err = service.AuthenticateAction(baseCtx, provider.delivery.secret)
	require.NoError(t, err, "durable generation catches up after successful host replacement")
}

func configurationBounds(desired model.DesiredConfiguration) model.ConfigurationBounds {
	return model.ConfigurationBounds{Harnesses: []string{desired.Harness}, Models: []string{desired.Model}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}
}

func testAccessService(store app.Store, provider ports.Provider) *app.Service {
	return app.New(store, fakeRegistry{provider: provider}).WithAgentAPIEndpoint("/tmp/tclaude-agent-api.sock")
}

type accessProvider struct {
	*fakeProvider
	delivery *fakeCredentialDelivery
}

func newAccessProvider() *accessProvider {
	return &accessProvider{fakeProvider: newFakeProvider(), delivery: &fakeCredentialDelivery{secrets: map[model.ExecutionID][]byte{}}}
}

func (p *accessProvider) ActionCredentials() ports.ActionCredentialDelivery { return p.delivery }

func (p *accessProvider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.fakeProvider.Prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	description := prepared.Describe()
	receipt, err := p.delivery.PrepareActionCredential(ctx, *request.ActionCredential)
	if err != nil {
		return nil, err
	}
	description.AccessDelivery = &receipt
	return &accessPrepared{PreparedAttempt: prepared, description: description}, nil
}

func (p *accessProvider) Recover(ctx context.Context, request ports.RecoveryRequest) (ports.RecoveryResult, error) {
	result, err := p.fakeProvider.Recover(ctx, request)
	if err != nil {
		return result, err
	}
	result.Attempt = request.Attempt
	if request.Access != nil {
		proof, err := p.delivery.InspectActionCredential(ctx, *request.Access)
		if err == nil {
			result.AccessProof = &proof
		}
	}
	return result, nil
}

type accessPrepared struct {
	ports.PreparedAttempt
	description ports.PreparedDescription
}

func (p *accessPrepared) Describe() ports.PreparedDescription { return p.description }

type fakeCredentialDelivery struct {
	secret  []byte
	secrets map[model.ExecutionID][]byte
}

func (d *fakeCredentialDelivery) PrepareActionCredential(_ context.Context, material ports.ActionCredentialMaterial) (ports.ActionCredentialReceipt, error) {
	if material.DeliveryID == "" {
		return ports.ActionCredentialReceipt{}, errors.New("delivery id is required")
	}
	d.secret = append([]byte(nil), material.Secret...)
	d.secrets[material.ExecutionID] = append([]byte(nil), material.Secret...)
	return credentialReceipt(material), nil
}

func (d *fakeCredentialDelivery) RotateActionCredential(_ context.Context, _ ports.ActionCredentialReceipt, material ports.ActionCredentialMaterial) (ports.ActionCredentialReceipt, error) {
	d.secret = append([]byte(nil), material.Secret...)
	d.secrets[material.ExecutionID] = append([]byte(nil), material.Secret...)
	return credentialReceipt(material), nil
}

func (d *fakeCredentialDelivery) InspectActionCredential(_ context.Context, binding model.ExecutionAccessBinding) (ports.ActionCredentialRecoveryProof, error) {
	return ports.ActionCredentialRecoveryProof{ExecutionID: binding.ExecutionID, Generation: binding.Generation, DeliveryID: binding.DeliveryID, Resource: "/private/credential", FileIdentity: "file-v" + string(rune(binding.Generation)), InspectedAt: time.Now()}, nil
}

func (d *fakeCredentialDelivery) RemoveActionCredential(_ context.Context, receipt ports.ActionCredentialReceipt) error {
	delete(d.secrets, receipt.ExecutionID)
	return nil
}

func credentialReceipt(material ports.ActionCredentialMaterial) ports.ActionCredentialReceipt {
	return ports.ActionCredentialReceipt{ExecutionID: material.ExecutionID, Generation: material.Generation, DeliveryID: material.DeliveryID, Resource: "/private/credential", FileIdentity: "file", DeliveredAt: time.Now()}
}
