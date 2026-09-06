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
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{
		Name: "message-recovery", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{
			Type: db.TriggerActionMessage, Message: &db.TriggerMessageAction{BodyTemplate: "recover"},
		}},
	})
	require.NoError(t, err)
	firingID, inserted, err := db.InsertTriggerFiring(ruleID, 1, events[0].ID, events[0].EventRef, "running", "", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, inserted)
	cause := messageCause{kind: messageCauseTrigger, firingID: firingID, actionIndex: 0}

	first, refused := acceptMessage(messagePrincipal{kind: messagePrincipalOperator},
		messageTarget{agentID: targetAgent}, messageContent{body: "recover"}, cause)
	require.Nil(t, refused)
	require.False(t, first.duplicate)
	require.Len(t, first.messageIDs, 1)

	// Model loss of the caller's result after commit. A retry must consult the
	// action outcome first: even an unusable selector cannot cause a reread or
	// a second inbox effect once acceptance is durable.
	recovered, refused := acceptMessage(messagePrincipal{},
		messageTarget{agentID: "now-missing"}, messageContent{body: "duplicate"}, cause)
	require.Nil(t, refused)
	require.True(t, recovered.duplicate)
	assert.Equal(t, first.messageIDs, recovered.messageIDs)
	messages, err := db.ListAgentMessagesForConv(targetConv, 10)
	require.NoError(t, err)
	assert.Len(t, messages, 1)
}
