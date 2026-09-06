package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerMessageAcceptanceDeduplicatesByActionOutcome(t *testing.T) {
	firingID, target := triggerMessageFixture(t)
	desired := TriggerActionOutcome{FiringID: firingID, ActionIndex: 0, ActionType: TriggerActionMessage, Outcome: "queued", CreatedAt: time.Now().UTC()}

	first, inserted, err := InsertTriggerMessageOutcome(&AgentMessage{ToConv: target, Body: "first"}, desired)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotZero(t, first.MessageID)

	duplicate, inserted, err := InsertTriggerMessageOutcome(&AgentMessage{ToConv: target, Body: "must not be inserted"}, desired)
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, first.MessageID, duplicate.MessageID)
	assert.Equal(t, first.Outcome, duplicate.Outcome)
	messages, err := ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "first", messages[0].Body)
}

func TestTriggerMessageAcceptanceRejectsConflictingActionIdentity(t *testing.T) {
	firingID, target := triggerMessageFixture(t)
	desired := TriggerActionOutcome{FiringID: firingID, ActionIndex: 2, ActionType: TriggerActionMessage, Outcome: "queued", CreatedAt: time.Now().UTC()}
	_, inserted, err := InsertTriggerMessageOutcome(&AgentMessage{ToConv: target, Body: "accepted"}, desired)
	require.NoError(t, err)
	require.True(t, inserted)

	desired.ActionType = TriggerActionSpawn
	_, inserted, err = InsertTriggerMessageOutcome(&AgentMessage{ToConv: target, Body: "conflict"}, desired)
	require.ErrorContains(t, err, "action identity conflict")
	assert.False(t, inserted)
	messages, err := ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	assert.Len(t, messages, 1)
}

func TestTriggerMessageAcceptancePreservesExistingTerminalOutcome(t *testing.T) {
	firingID, target := triggerMessageFixture(t)
	terminal := TriggerActionOutcome{
		FiringID: firingID, ActionIndex: 1, ActionType: TriggerActionMessage,
		Outcome: "permission_denied", Detail: "captured denial", CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, InsertTriggerActionOutcome(&terminal))
	desired := terminal
	desired.Outcome, desired.Detail = "queued", ""

	stored, inserted, err := InsertTriggerMessageOutcome(&AgentMessage{ToConv: target, Body: "must not be inserted"}, desired)
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, "permission_denied", stored.Outcome)
	assert.Equal(t, "captured denial", stored.Detail)
	assert.Zero(t, stored.MessageID)
	messages, err := ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	assert.Empty(t, messages)
}

func TestTriggerMessageAcceptanceRollsBackMessageWithOutcomeFailure(t *testing.T) {
	firingID, target := triggerMessageFixture(t)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TRIGGER fail_trigger_message_outcome
		BEFORE UPDATE OF message_id ON trigger_action_outcomes
		BEGIN SELECT RAISE(ABORT, 'injected outcome failure'); END`)
	require.NoError(t, err)

	desired := TriggerActionOutcome{FiringID: firingID, ActionIndex: 0, ActionType: TriggerActionMessage, Outcome: "queued", CreatedAt: time.Now().UTC()}
	_, inserted, err := InsertTriggerMessageOutcome(&AgentMessage{ToConv: target, Body: "rolled back"}, desired)
	require.ErrorContains(t, err, "injected outcome failure")
	assert.False(t, inserted)
	messages, err := ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	assert.Empty(t, messages)
	outcomes, err := ListTriggerActionOutcomes(firingID)
	require.NoError(t, err)
	assert.Empty(t, outcomes)
}

func triggerMessageFixture(t *testing.T) (int64, string) {
	t.Helper()
	setupTestDB(t)
	const target = "trigger-message-target"
	agentID, _, err := EnsureAgentForConv(target, "test")
	require.NoError(t, err)
	_, err = UpsertAgentPR(agentID, "https://github.com/o/r/pull/777", "ready", "open")
	require.NoError(t, err)
	_, err = ReconcileTriggerPREvents()
	require.NoError(t, err)
	events, err := ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	ruleID, err := InsertTriggerRule(&TriggerRule{
		Name: "message-atomicity", Enabled: true, OperatorAuthored: true,
		ScopeKind: TriggerScopeGlobal, Source: TriggerSourcePROpened, DraftFilter: TriggerDraftInclude,
		Actions: []TriggerAction{{Type: TriggerActionMessage, Message: &TriggerMessageAction{BodyTemplate: "review"}}},
	})
	require.NoError(t, err)
	firingID, inserted, err := InsertTriggerFiring(ruleID, 1, events[0].ID, events[0].EventRef, "running", "", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, inserted)
	return firingID, target
}
