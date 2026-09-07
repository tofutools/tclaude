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
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestDurableThreadAudienceAttachmentRetirementAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "collaboration.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	service := testService(store, newFakeProvider())
	operator := model.OperatorPrincipal()

	parent := createAgent(t, ctx, service, operator, "agent_parent")
	created, err := service.CreateAgent(ctx, app.CreateAgentRequest{
		Context: operator, ID: "agent_alice", Name: "Alice", TaskReference: "https://tracker.example/T-42",
		ParentAgentID: parent.ID, CloneSourceAgentID: parent.ID,
		Notifications: model.AgentNotificationPreferences{DirectMessage: model.NotificationNone},
		Desired:       parent.Desired,
	})
	require.NoError(t, err)
	alice := created.Agent
	bob := createAgent(t, ctx, service, operator, "agent_bob")
	charlie := createAgent(t, ctx, service, operator, "agent_charlie")
	require.Equal(t, parent.ID, alice.ParentAgentID)
	require.Equal(t, parent.ID, alice.CloneSourceAgentID)
	require.Equal(t, "https://tracker.example/T-42", alice.TaskReference)
	require.Equal(t, model.NotificationNone, alice.Notifications.DirectMessage)

	rootRequest := app.SendMessageRequest{
		RequestContext: effect(operator, "message_root"), Subject: "Review", Body: "Please review the attached patch.",
		To: model.MessageAudience{AgentIDs: []model.AgentID{bob.ID}}, CC: model.MessageAudience{Operator: true},
		Attachments: []app.AttachmentInput{{Filename: "patch.diff", MediaType: "text/x-diff", Content: []byte("diff --git a/a b/a\n")}},
	}
	root, err := service.SendMessage(ctx, rootRequest)
	require.NoError(t, err)
	require.Equal(t, root.Message.ID, root.Message.ThreadID)
	require.Len(t, root.Message.Recipients, 2)
	require.Len(t, root.Message.Attachments, 1)
	require.Equal(t, int64(len("diff --git a/a b/a\n")), root.Message.Attachments[0].Size)
	require.NotEmpty(t, root.Message.Attachments[0].SHA256)
	changedAttachment := rootRequest
	changedAttachment.Attachments = []app.AttachmentInput{{Filename: "patch.diff", MediaType: "text/x-diff", Content: []byte("different bytes")}}
	_, err = service.SendMessage(ctx, changedAttachment)
	require.ErrorIs(t, err, app.ErrConflict, "RequestID binds attachment digests")
	changedAudience := rootRequest
	changedAudience.To = model.MessageAudience{AgentIDs: []model.AgentID{alice.ID}}
	_, err = service.SendMessage(ctx, changedAudience)
	require.ErrorIs(t, err, app.ErrConflict, "RequestID binds the authored audience")
	changedParent := rootRequest
	changedParent.ParentMessageID = root.Message.ID
	_, err = service.SendMessage(ctx, changedParent)
	require.ErrorIs(t, err, app.ErrConflict, "RequestID binds the thread parent")
	var bobRecipient model.MessageRecipient
	for _, recipient := range root.Message.Recipients {
		if recipient.AgentID == bob.ID {
			bobRecipient = recipient
		}
	}
	require.Equal(t, model.NotificationPending, bobRecipient.NotificationOutcome)
	notified, err := service.RecordMessageNotification(ctx, app.MessageNotificationUpdate{RecipientID: bobRecipient.ID, Outcome: model.NotificationUnavailable, Detail: "recipient offline"})
	require.NoError(t, err)
	for _, recipient := range notified.Message.Recipients {
		if recipient.AgentID == bob.ID {
			require.Equal(t, model.NotificationUnavailable, recipient.NotificationOutcome)
			require.NotNil(t, recipient.NotifiedAt)
		}
	}

	content, err := service.ReadAttachment(ctx, app.ReadAttachmentRequest{Principal: model.AgentPrincipal(bob.ID), AttachmentID: root.Message.Attachments[0].ID})
	require.NoError(t, err)
	require.Equal(t, []byte("diff --git a/a b/a\n"), content.Content)
	_, err = service.ReadAttachment(ctx, app.ReadAttachmentRequest{Principal: model.AgentPrincipal(charlie.ID), AttachmentID: root.Message.Attachments[0].ID})
	require.ErrorIs(t, err, app.ErrUnauthorized)

	read, err := service.MarkMessageRead(ctx, app.MarkMessageReadRequest{RequestContext: effect(model.AgentPrincipal(bob.ID), "read_root"), MessageID: root.Message.ID})
	require.NoError(t, err)
	require.NotNil(t, read.Message.Recipients[0].ReadAt)
	_, err = service.MarkMessageRead(ctx, app.MarkMessageReadRequest{RequestContext: effect(operator, "read_operator_copy"), MessageID: root.Message.ID, Operator: true})
	require.NoError(t, err)

	claim, err := service.CreateAttachmentClaim(ctx, app.CreateAttachmentClaimRequest{Principal: operator, Filename: "notes.txt", MediaType: "text/plain", Content: []byte("owned once")})
	require.NoError(t, err)
	replyRequest := app.SendMessageRequest{
		RequestContext: effect(operator, "message_reply"), Subject: "Re: Review", Body: "Follow-up", ParentMessageID: root.Message.ID,
		To: model.MessageAudience{AgentIDs: []model.AgentID{alice.ID}},
		Attachments: []app.AttachmentInput{{Claim: &app.AttachmentClaimReference{
			ClaimID: claim.Claim.ID, AttachmentID: claim.Claim.Attachment.ID, Filename: claim.Claim.Attachment.Filename,
			MediaType: claim.Claim.Attachment.MediaType, Size: claim.Claim.Attachment.Size, SHA256: claim.Claim.Attachment.SHA256,
		}}},
	}
	reply, err := service.SendMessage(ctx, replyRequest)
	require.NoError(t, err)
	require.Equal(t, root.Message.ID, reply.Message.ParentMessageID)
	require.Equal(t, root.Message.ID, reply.Message.ThreadID)
	require.Equal(t, model.NotificationNotRequested, reply.Message.Recipients[0].NotificationOutcome, "the authored agent preference is visible and creates no notification authority")
	repeatedReply, err := service.SendMessage(ctx, replyRequest)
	require.NoError(t, err, "an exact retry returns the accepted message before rereading the consumed claim")
	require.Equal(t, reply.Message.ID, repeatedReply.Message.ID)

	retired, err := service.RetireAgent(ctx, app.RetireAgentRequest{Context: operator, ID: bob.ID, ExpectedRevision: bob.Revision, Reason: "review complete"})
	require.NoError(t, err)
	require.Equal(t, model.AgentRetired, retired.Agent.Lifecycle)
	require.NotNil(t, retired.Agent.RetiredAt)
	require.Equal(t, "review complete", retired.Agent.RetirementReason)
	_, err = service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(model.AgentPrincipal(bob.ID), "retired_send"), To: model.MessageAudience{Operator: true}, Body: "must fail"})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	repeatedRoot, err := service.SendMessage(ctx, rootRequest)
	require.NoError(t, err, "an exact retry precedes the now-retired audience reread")
	require.Equal(t, root.Message.ID, repeatedRoot.Message.ID)

	stale := retired.Agent.Revision - 1
	_, err = service.ReactivateAgent(ctx, app.ReactivateAgentRequest{Context: operator, ID: bob.ID, ExpectedRevision: stale})
	require.ErrorIs(t, err, app.ErrConflict)
	reactivated, err := service.ReactivateAgent(ctx, app.ReactivateAgentRequest{Context: operator, ID: bob.ID, ExpectedRevision: retired.Agent.Revision})
	require.NoError(t, err)
	require.Equal(t, model.AgentActive, reactivated.Agent.Lifecycle)
	require.Empty(t, reactivated.Agent.PrimaryExecutionID, "reactivation does not resurrect a runtime")

	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	reopened := testService(store, newFakeProvider())
	snapshot, err := reopened.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 2)
	require.Len(t, snapshot.Messages[0].Attachments, 1)
	require.Equal(t, root.Message.ID, snapshot.Messages[1].ThreadID)
	require.Equal(t, model.AgentActive, findAgent(t, snapshot.Agents, bob.ID).Lifecycle)
}

func TestRetirementRefusesLivePrimaryThenRevokesAndBlocksNewEffects(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "retire.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	service := testService(store, newFakeProvider())
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_worker")

	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_worker"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	agent = findAgent(t, snapshot.Agents, agent.ID)
	_, err = service.RetireAgent(ctx, app.RetireAgentRequest{Context: operator, ID: agent.ID, ExpectedRevision: agent.Revision, Reason: "done"})
	require.ErrorIs(t, err, app.ErrConflict, "retirement cannot orphan a running primary")

	_, err = service.Stop(ctx, app.StopRequest{RequestContext: effect(operator, "stop_worker"), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	retired, err := service.RetireAgent(ctx, app.RetireAgentRequest{Context: operator, ID: agent.ID, ExpectedRevision: agent.Revision, Reason: "done"})
	require.NoError(t, err)
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "relaunch_retired"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: retired.Agent.Revision}}})
	require.ErrorIs(t, err, app.ErrConflict)

	access, err := store.ExecutionAccess(ctx, launched.Execution.ID)
	if !errors.Is(err, app.ErrNotFound) {
		require.NoError(t, err)
		require.Equal(t, model.ExecutionAccessRevoked, access.State)
	}
	snapshot, err = service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Executions, 1, "retirement retains execution history")
	require.Equal(t, model.AgentRetired, findAgent(t, snapshot.Agents, agent.ID).Lifecycle)
}

func TestDelegatedRetirementRequiresCurrentExactAgentAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "delegated-retire.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	service := testService(store, newFakeProvider())
	operator := model.OperatorPrincipal()
	manager := createAgent(t, ctx, service, operator, "retirement_manager")
	permitted := createAgent(t, ctx, service, operator, "permitted_target")
	denied := createAgent(t, ctx, service, operator, "denied_target")
	revoked := createAgent(t, ctx, service, operator, "revoked_target")
	now := time.Now().UTC()

	putRetirementGrant := func(id model.GrantID, target model.AgentID) model.AuthorityGrant {
		grant, putErr := store.PutGrant(ctx, model.AuthorityGrant{
			ID: id, Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: manager.ID},
			Action: model.ActionRetireAgent, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target},
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}, 0)
		require.NoError(t, putErr)
		return grant
	}

	putRetirementGrant("permit_retirement", permitted.ID)
	retired, err := service.RetireAgent(ctx, app.RetireAgentRequest{Context: model.AgentPrincipal(manager.ID), ID: permitted.ID, ExpectedRevision: permitted.Revision, Reason: "delegated cleanup"})
	require.NoError(t, err)
	require.Equal(t, model.AgentRetired, retired.Agent.Lifecycle)

	_, err = service.RetireAgent(ctx, app.RetireAgentRequest{Context: model.AgentPrincipal(manager.ID), ID: denied.ID, ExpectedRevision: denied.Revision, Reason: "must be denied"})
	require.ErrorIs(t, err, app.ErrUnauthorized)

	revokedGrant := putRetirementGrant("revoke_retirement", revoked.ID)
	require.NoError(t, store.DeleteGrant(ctx, revokedGrant.ID, revokedGrant.Revision))
	_, err = service.RetireAgent(ctx, app.RetireAgentRequest{Context: model.AgentPrincipal(manager.ID), ID: revoked.ID, ExpectedRevision: revoked.Revision, Reason: "revoked before retirement"})
	require.ErrorIs(t, err, app.ErrUnauthorized, "retirement rechecks the live grant in its mutation transaction")
}

func findAgent(t *testing.T, agents []model.Agent, id model.AgentID) model.Agent {
	t.Helper()
	for _, agent := range agents {
		if agent.ID == id {
			return agent
		}
	}
	t.Fatalf("agent %s not found", id)
	return model.Agent{}
}
