package app_test

import (
	"context"
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

	sent, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(callerA, "same_request"), RecipientAgentIDs: []model.AgentID{b.ID}, Body: "offline"})
	require.NoError(t, err)
	status, err := service.ExecutionAccessStatus(ctx, app.ExecutionAccessStatusRequest{Principal: operator, ExecutionID: launchedA.Execution.ID})
	require.NoError(t, err)
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
	deliveryID := material.DeliveryID
	if deliveryID == "" {
		deliveryID = "delivery_" + string(material.ExecutionID)
	}
	return ports.ActionCredentialReceipt{ExecutionID: material.ExecutionID, Generation: material.Generation, DeliveryID: deliveryID, Resource: "/private/credential", FileIdentity: "file", DeliveredAt: time.Now()}
}
