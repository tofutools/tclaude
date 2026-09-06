package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestMessageNotificationDeliveryCancellationAndRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "notifications.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	provider := newFakeProvider()
	service := testService(store, provider)
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "recipient")
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	message, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(operator, "send"), RecipientAgentIDs: []model.AgentID{agent.ID}, Body: "Durable message"})
	require.NoError(t, err)
	provider.runtime.onInteract = cancel
	require.NoError(t, service.ReconcileMessageNotifications(ctx))
	require.Equal(t, 1, provider.runtime.interactions)
	inbox, err := service.ReadInbox(context.Background(), app.ReadInboxRequest{Principal: model.AgentPrincipal(agent.ID)})
	require.NoError(t, err)
	require.Equal(t, message.Message.ID, inbox.Messages[0].ID)
	require.Equal(t, model.NotificationDelivered, inbox.Messages[0].Recipients[0].NotificationOutcome)
	require.Nil(t, inbox.Messages[0].Recipients[0].ReadAt, "native notice is not an inbox read or work completion")
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	restarted := testService(store, provider)
	require.NoError(t, restarted.ReconcileMessageNotifications(context.Background()))
	require.Equal(t, 1, provider.runtime.interactions, "settled notice is never replayed")
}

func TestMessageNotificationRechecksAuthorityAndExactRecipient(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "notifications.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	operator := model.OperatorPrincipal()
	sender := createAgent(t, ctx, service, operator, "sender")
	recipient := createAgent(t, ctx, service, operator, "recipient")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: recipient.ID, ExpectedRevision: recipient.Revision}}})
	require.NoError(t, err)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "send_grant", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: sender.ID}, Action: model.ActionSendMessage, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: recipient.ID}}})
	require.NoError(t, err)
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(model.AgentPrincipal(sender.ID), "send"), RecipientAgentIDs: []model.AgentID{recipient.ID}, Body: "accepted before revocation"})
	require.NoError(t, err)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	require.NoError(t, service.ReconcileMessageNotifications(ctx))
	require.Zero(t, provider.runtime.interactions)
	inbox, err := service.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(recipient.ID)})
	require.NoError(t, err)
	require.Equal(t, model.NotificationUnavailable, inbox.Messages[0].Recipients[0].NotificationOutcome)
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(operator, "send_late"), RecipientAgentIDs: []model.AgentID{recipient.ID}, Body: "accepted before stop"})
	require.NoError(t, err)
	candidates, err := store.PendingMessageNotifications(ctx, 64)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: effect(operator, "stop"), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	require.ErrorIs(t, store.ConsumeMessageNotification(ctx, candidates[0], inbox.Messages[0].CreatedAt), app.ErrConflict)
	require.NoError(t, service.ReconcileMessageNotifications(ctx))
	require.Zero(t, provider.runtime.interactions)
}

func TestMessageNotificationConsumedCrashRemainsUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notifications.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	provider := newFakeProvider()
	service := testService(store, provider)
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "recipient")
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	sent, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(operator, "send"), RecipientAgentIDs: []model.AgentID{agent.ID}, Body: "durable regardless of notification"})
	require.NoError(t, err)
	candidates, err := store.PendingMessageNotifications(ctx, 64)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.NoError(t, store.ConsumeMessageNotification(ctx, candidates[0], sent.Message.CreatedAt))
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	restarted := testService(store, provider)
	require.NoError(t, restarted.ReconcileMessageNotifications(ctx))
	require.Zero(t, provider.runtime.interactions)
	inbox, err := restarted.ReadInbox(ctx, app.ReadInboxRequest{Principal: model.AgentPrincipal(agent.ID)})
	require.NoError(t, err)
	require.Equal(t, model.NotificationUnknown, inbox.Messages[0].Recipients[0].NotificationOutcome)
	require.Equal(t, "durable regardless of notification", inbox.Messages[0].Body)
}
