package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerDwellStateAndEventEvidenceRoundTrip(t *testing.T) {
	setupTestDB(t)
	rule := &TriggerRule{Name: "idle-worker", Enabled: true, OperatorAuthored: true,
		ScopeKind: TriggerScopeGlobal, Source: TriggerSourceAgentIdle, DraftFilter: TriggerDraftInclude,
		ForSeconds: 60, Actions: []TriggerAction{{Type: TriggerActionMessage,
			Message: &TriggerMessageAction{BodyTemplate: "wake {{agent.id}}"}}}}
	ruleID, err := InsertTriggerRule(rule)
	require.NoError(t, err)
	rule, err = GetTriggerRule(ruleID)
	require.NoError(t, err)
	assert.Equal(t, "agent", rule.Actions[0].Message.Target)
	agentID, _, err := EnsureAgentForConv("dwell-conv", "test")
	require.NoError(t, err)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	state := TriggerDwellState{RuleID: ruleID, AgentID: agentID, RuleRevision: rule.Revision,
		Episode: 1, Result: "true", Harness: "claude", FactObservedAt: now,
		TrueSince: now.Add(-time.Minute), FiredAt: now, UpdatedAt: now}
	emitted, err := ApplyTriggerDwellState(rule, state, "last harness hook; messages and pane input excluded", "claude", now, now, true)
	require.NoError(t, err)
	require.True(t, emitted)

	stored, err := GetTriggerDwellState(ruleID, agentID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "true", stored.Result)
	assert.Equal(t, now.Add(-time.Minute), stored.TrueSince)
	events, err := ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Zero(t, events[0].AgentPRID)
	assert.Equal(t, TriggerSourceAgentIdle, events[0].Source)
	assert.Equal(t, agentID, events[0].AgentID)
	assert.Equal(t, "claude", events[0].AgentHarness)
	assert.Equal(t, "true", events[0].FactResult)
	assert.Equal(t, now.Add(-time.Minute), events[0].DwellStartedAt)

	_, inserted, err := InsertTriggerFiring(ruleID, rule.Revision, events[0].ID, events[0].EventRef, "ok", "", now)
	require.NoError(t, err)
	require.True(t, inserted)
	firings, err := ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	assert.Equal(t, agentID, firings[0].AgentID)
	assert.Equal(t, "true", firings[0].FactResult)
	assert.Equal(t, now.Add(-time.Minute), firings[0].DwellStartedAt)
}

func TestTriggerDwellValidationIsSourceSpecific(t *testing.T) {
	message := []TriggerAction{{Type: TriggerActionMessage, Message: &TriggerMessageAction{BodyTemplate: "x"}}}
	state := &TriggerRule{Name: "state", Enabled: true, OperatorAuthored: true, ScopeKind: TriggerScopeGlobal,
		Source: TriggerSourceAgentAwaitingInput, DraftFilter: TriggerDraftInclude, Actions: message}
	assert.ErrorContains(t, state.Validate(), "for_seconds")
	state.ForSeconds = 10
	require.NoError(t, state.Validate())
	pr := &TriggerRule{Name: "pr", Enabled: true, OperatorAuthored: true, ScopeKind: TriggerScopeGlobal,
		Source: TriggerSourcePROpened, DraftFilter: TriggerDraftInclude, ForSeconds: 10, Actions: message}
	assert.ErrorContains(t, pr.Validate(), "only valid")
}

func TestTriggerRuleSourceChangeClearsIncompatibleDwellState(t *testing.T) {
	setupTestDB(t)
	rule := &TriggerRule{Name: "source-change", Enabled: true, OperatorAuthored: true,
		ScopeKind: TriggerScopeGlobal, Source: TriggerSourceAgentIdle, DraftFilter: TriggerDraftInclude,
		ForSeconds: 60, Actions: []TriggerAction{{Type: TriggerActionMessage,
			Message: &TriggerMessageAction{Target: "agent", BodyTemplate: "wake"}}}}
	id, err := InsertTriggerRule(rule)
	require.NoError(t, err)
	rule, err = GetTriggerRule(id)
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = ApplyTriggerDwellState(rule, TriggerDwellState{RuleID: id, AgentID: "agt_1",
		RuleRevision: rule.Revision, Episode: 1, Result: "true", TrueSince: now},
		"idle", "claude", now, now, false)
	require.NoError(t, err)

	rule.Source = TriggerSourceAgentAwaitingInput
	require.NoError(t, UpdateTriggerRule(id, rule.RowVersion, rule))
	state, err := GetTriggerDwellState(id, "agt_1")
	require.NoError(t, err)
	assert.Nil(t, state)
}
