package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestMessageAdmissionTriggerRecoversCommittedAcceptanceWithoutReresolvingTarget(t *testing.T) {
	setupTestDB(t)
	const targetConv = "message-admission-recovery-target"
	targetAgent, _, err := db.EnsureAgentForConv(targetConv, "test")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(targetAgent, "https://github.com/o/r/pull/999", "ready", "open")
	require.NoError(t, err)
	_, err = db.ReconcileTriggerPREvents()
	require.NoError(t, err)
	events, err := db.ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	rule := &db.TriggerRule{
		Name: "message-recovery", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{
			Type: db.TriggerActionMessage, Message: &db.TriggerMessageAction{BodyTemplate: "recover"},
		}},
	}
	ruleID, err := db.InsertTriggerRule(rule)
	require.NoError(t, err)
	rule.ID = ruleID
	firingID, inserted, err := db.InsertTriggerFiring(ruleID, 1, events[0].ID, events[0].EventRef, "running", "", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, inserted)
	outcome, detail, firstID, recorded := executeTriggerMessage(rule, firingID, 0, rule.Actions[0].Message, events[0])
	require.Equal(t, "queued", outcome, detail)
	require.True(t, recorded)
	require.NotZero(t, firstID)

	// Model loss of the caller's result after commit. A retry must consult the
	// action outcome first: even unusable owner and target sources cannot cause
	// a reread or a second inbox effect once acceptance is durable.
	rule.OperatorAuthored = false
	rule.OwnerAgent = "now-missing"
	events[0].PRAuthorAgent = "also-missing"
	outcome, detail, recoveredID, recorded := executeTriggerMessage(rule, firingID, 0, nil, events[0])
	require.Equal(t, "queued", outcome, detail)
	require.True(t, recorded)
	assert.Equal(t, firstID, recoveredID)
	messages, err := db.ListAgentMessagesForConv(targetConv, 10)
	require.NoError(t, err)
	assert.Len(t, messages, 1)
}
