package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

type retireBeforeCollaborationAdmissionStore struct {
	app.Store
	target model.Agent
}

func (s *retireBeforeCollaborationAdmissionStore) CreateMessage(ctx context.Context, in app.MessageAdmission) (app.MessageAdmissionResult, error) {
	_, err := s.RetireAgent(ctx, s.target.ID, s.target.Revision, model.OperatorPrincipal(), "retired concurrently", time.Now())
	if err != nil {
		return app.MessageAdmissionResult{}, err
	}
	return s.Store.CreateMessage(ctx, in)
}
func TestFreshMessageRefusesRecipientRetiredAtAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer store.Close()
	service := testService(store, newFakeProvider())
	op := model.OperatorPrincipal()
	target := createAgent(t, ctx, service, op, "agent_target")
	service = testService(&retireBeforeCollaborationAdmissionStore{Store: store, target: target}, newFakeProvider())
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(op, "race_send"), To: model.MessageAudience{AgentIDs: []model.AgentID{target.ID}}, Body: "new effect"})
	require.Error(t, err, "fresh message committed after its only recipient retired")
}
func TestInboxKeepsPublicToCCAddresses(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer store.Close()
	service := testService(store, newFakeProvider())
	op := model.OperatorPrincipal()
	alice := createAgent(t, ctx, service, op, "agent_alice")
	bob := createAgent(t, ctx, service, op, "agent_bob")
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(op, "audience_send"), To: model.MessageAudience{AgentIDs: []model.AgentID{alice.ID}}, CC: model.MessageAudience{AgentIDs: []model.AgentID{bob.ID}, Operator: true}, Body: "review together"})
	require.NoError(t, err)
	_, err = service.MarkMessageRead(ctx, app.MarkMessageReadRequest{RequestContext: effect(model.AgentPrincipal(bob.ID), "bob_reads"), MessageID: func() model.MessageID {
		snap, e := service.Snapshot(ctx, app.SnapshotRequest{Principal: op})
		require.NoError(t, e)
		return snap.Messages[0].ID
	}(), AgentID: bob.ID})
	require.NoError(t, err)
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(alice.ID)})
	require.NoError(t, err)
	require.Len(t, inbox.Messages[0].Recipients, 3, "recipient cannot discover public CC addresses to reply all")
	for _, r := range inbox.Messages[0].Recipients {
		if r.AgentID != alice.ID {
			require.Nil(t, r.ReadAt)
			require.Empty(t, r.NotificationOutcome)
			require.Empty(t, r.NotificationDetail)
			require.Empty(t, r.ID)
		}
	}
}
func TestAttachmentReadsRequireLiveDelegation(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer store.Close()
	now := time.Now()
	service := testService(store, newFakeProvider()).WithClock(func() time.Time { return now })
	principal := model.AutomationPrincipal("run_attachment", model.AuthoritySubject{Kind: model.AuthorityOperator}, model.AutomationDelegation{Actions: []model.Action{model.ActionSendMessage, model.ActionReadAttachment}, Resources: []model.ResourceSelector{{Kind: model.ResourceOperator}}, ExpiresAt: now.Add(time.Minute)})
	sent, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(principal, "automation_attach"), To: model.MessageAudience{Operator: true}, Attachments: []app.AttachmentInput{{Filename: "note.txt", Content: []byte("private correspondence")}}})
	require.NoError(t, err)
	_, err = service.ReadAttachment(ctx, app.ReadAttachmentRequest{Principal: principal, AttachmentID: sent.Message.Attachments[0].ID})
	require.NoError(t, err, "explicit live read delegation works")
	wrongAction := principal
	delegation := *principal.Delegation
	delegation.Actions = []model.Action{model.ActionSendMessage}
	wrongAction.Delegation = &delegation
	_, err = service.ReadAttachment(ctx, app.ReadAttachmentRequest{Principal: wrongAction, AttachmentID: sent.Message.Attachments[0].ID})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	wrongResource := principal
	other := *principal.Delegation
	other.Resources = []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "unrelated"}}
	wrongResource.Delegation = &other
	_, err = service.ReadAttachment(ctx, app.ReadAttachmentRequest{Principal: wrongResource, AttachmentID: sent.Message.Attachments[0].ID})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = service.CreateAttachmentClaim(ctx, app.CreateAttachmentClaimRequest{Principal: principal, Filename: "claim.txt", Content: []byte("pending")})
	require.NoError(t, err)
	now = now.Add(2 * time.Minute)
	_, err = service.CreateAttachmentClaim(ctx, app.CreateAttachmentClaimRequest{Principal: principal, Filename: "claim.txt", Content: []byte("pending")})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: principal, Action: model.ActionSendMessage, Resource: model.ResourceSelector{Kind: model.ResourceOperator}}, now)
	require.NoError(t, err)
	require.False(t, decision.Allowed)
	_, err = service.ReadAttachment(ctx, app.ReadAttachmentRequest{Principal: principal, AttachmentID: sent.Message.Attachments[0].ID})
	require.ErrorIs(t, err, app.ErrUnauthorized, "expired automation delegation still downloads attachment")
}
