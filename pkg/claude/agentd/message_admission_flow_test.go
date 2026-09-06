package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestMessageAdmissionDirectPreservesHTTPAttributionAndCapacity(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("admission-direct")
	const sender, target = "admission-sender", "admission-target"
	f.HaveMember("admission-direct", sender)
	f.HaveMember("admission-direct", target)

	rec := postMessage(t, f, sender, map[string]any{"to": target, "body": "accepted direct"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	messages, err := db.ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, sender, messages[0].FromConv)
	assert.True(t, messages[0].RegularSend)
	assert.Equal(t, []string{target}, messages[0].ToRecipients)
	assert.Equal(t, "admission-direct", groupNameForID(t, messages[0].GroupID))
}

func TestMessageAdmissionTriggerCommitsOutcomeAndBypassesRegularCapacity(t *testing.T) {
	f := triggerFlow(t)
	g := f.HaveGroup("admission-trigger")
	const target = "admission-trigger-target"
	f.HaveConvWithTitle(target, "target")
	f.HaveMember("admission-trigger", target)
	seedRegularBacklog(t, target, regularMessageQueueLimitForTest)

	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{
		Name: "admission-trigger-message", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{
			Type:    db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{Target: "group", BodyTemplate: "atomic trigger message"},
		}},
	})
	require.NoError(t, err)
	agentID, err := db.AgentIDForConv(target)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentID, "https://github.com/o/r/pull/888", "ready", "open")
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().UTC().Add(time.Second))

	firings, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	require.Len(t, firings[0].Actions, 1)
	action := firings[0].Actions[0]
	assert.Equal(t, "queued", action.Outcome)
	require.NotZero(t, action.MessageID)
	message, err := db.GetAgentMessage(action.MessageID)
	require.NoError(t, err)
	require.NotNil(t, message)
	assert.Equal(t, target, message.ToConv, "target=group still selects the event agent")
	assert.Equal(t, g.ID, message.GroupID)
	assert.False(t, message.RegularSend, "internal trigger traffic is capacity-exempt")
	assert.True(t, db.IsOperatorAgentMessage(message.ID))
	messages, err := db.ListAgentMessagesForConv(target, 20)
	require.NoError(t, err)
	assert.Len(t, messages, regularMessageQueueLimitForTest+1)
}

func TestMessageAdmissionAgentOwnedTriggerMayNotifyOwnPRAuthor(t *testing.T) {
	f := triggerFlow(t)
	g := f.HaveGroup("admission-self-trigger")
	const owner = "admission-self-trigger-owner"
	f.HaveConvWithTitle(owner, "owner")
	f.HaveMember("admission-self-trigger", owner)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"))
	ownerAgent, err := db.AgentIDForConv(owner)
	require.NoError(t, err)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{
		Name: "admission-self-trigger", Enabled: true, OwnerAgent: ownerAgent,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{
			Type:    db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{Target: "pr.author_agent", BodyTemplate: "your PR opened"},
		}},
	})
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(ownerAgent, "https://github.com/o/r/pull/999", "ready", "open")
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().UTC().Add(time.Second))

	firings, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	require.Len(t, firings[0].Actions, 1)
	assert.Equal(t, "queued", firings[0].Actions[0].Outcome, firings[0].Actions[0].Detail)
	messages, err := db.ListAgentMessagesForConv(owner, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, owner, messages[0].FromConv)
	assert.Equal(t, owner, messages[0].ToConv)
}

func groupNameForID(t *testing.T, id int64) string {
	t.Helper()
	g, err := db.GetAgentGroupByID(id)
	require.NoError(t, err)
	require.NotNil(t, g)
	return g.Name
}
