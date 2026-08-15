package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func triggerMessageBody(group string, debounce int64) map[string]any {
	return map[string]any{
		"name": "review-nudge", "enabled": true, "scope": "group", "group": group,
		"source": "pr.opened", "draft_filter": "exclude", "debounce_seconds": debounce,
		"actions": []map[string]any{{"type": "message", "message": map[string]any{
			"target": "pr.author_agent", "subject_template": "Review PR {{pr.number}}",
			"body_template": "Please review {{pr.url}} for {{group}}.",
		}}},
	}
}

func TestDashboardTriggersCRUDContract(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost, "/api/triggers", triggerMessageBody("alpha", 5)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID         int64  `json:"id"`
		RowVersion int64  `json:"row_version"`
		Revision   int64  `json:"revision"`
		Name       string `json:"name"`
		Group      string `json:"group"`
	}
	testharness.DecodeJSON(t, rec, &created)
	assert.Positive(t, created.ID)
	assert.Equal(t, int64(1), created.RowVersion)
	assert.Equal(t, "alpha", created.Group)
	rec = testharness.Serve(dash, dashReq(t, http.MethodGet, fmt.Sprintf("/api/triggers/%d", created.ID), nil))
	require.Equal(t, 200, rec.Code)
	body := triggerMessageBody("alpha", 10)
	body["row_version"] = created.RowVersion
	body["name"] = "review-nudge-v2"
	rec = testharness.Serve(dash, dashReq(t, http.MethodPatch, fmt.Sprintf("/api/triggers/%d", created.ID), body))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = testharness.Serve(dash, dashReq(t, http.MethodPatch, fmt.Sprintf("/api/triggers/%d", created.ID), body))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestTriggerReadPermissionsFilterGlobalAndGroupRules(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	const owner = "trigger-reader-conv"
	f.HaveConvWithTitle(owner, "reader")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"))
	groupRule := &db.TriggerRule{Name: "alpha-only", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{BodyTemplate: "review {{pr.url}}"}}}}
	_, err := db.InsertTriggerRule(groupRule)
	require.NoError(t, err)
	globalRule := &db.TriggerRule{Name: "global", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{BodyTemplate: "review {{pr.url}}"}}}}
	_, err = db.InsertTriggerRule(globalRule)
	require.NoError(t, err)

	request := func() []string {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodGet, "/v1/triggers", nil), owner))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Triggers []struct{ Name string } `json:"triggers"`
		}
		testharness.DecodeJSON(t, rec, &body)
		names := make([]string, 0, len(body.Triggers))
		for _, trigger := range body.Triggers {
			names = append(names, trigger.Name)
		}
		return names
	}
	got := request()
	require.Len(t, got, 1)
	assert.Equal(t, "alpha-only", got[0], "group ownership must not confer global read")
	require.NoError(t, db.GrantAgentPermission(owner, agentd.PermTriggersRead, "test"))
	got = request()
	require.Len(t, got, 2)
}

func TestTriggerPROpenedDebounceRestartDedupAndMessageLedger(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const author = "trig-author-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("alpha", author)
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost, "/api/triggers", triggerMessageBody("alpha", 60)))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var created struct{ ID int64 }
	testharness.DecodeJSON(t, rec, &created)
	agentID, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	base := time.Now().UTC()
	_, err = db.UpsertAgentPRDetails(agentID, "https://github.com/tofutools/tclaude/pull/456", "ready", "open", "feature", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(base.Add(time.Second))
	rows, err := db.ListTriggerFirings(created.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, rows, "debounce defers action")
	_, err = db.UpsertAgentPRDetails(agentID, "https://github.com/tofutools/tclaude/pull/456", "edited", "open", "feature", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(base.Add(2 * time.Minute))
	rows, err = db.ListTriggerFirings(created.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "ok", rows[0].Outcome)
	require.Len(t, rows[0].Actions, 1)
	assert.Equal(t, "queued", rows[0].Actions[0].Outcome)
	msgs, err := db.ListAgentMessagesForConv(author, 20)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Body, "pull/456")
	// A second scheduler instance after restart sees the same durable event and
	// firing uniqueness; neither source reconciliation nor the tick replays it.
	agentd.RunTriggerTickForTest(base.Add(3 * time.Minute))
	rows, err = db.ListTriggerFirings(created.ID, 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	msgs, err = db.ListAgentMessagesForConv(author, 20)
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
}

func TestTriggerDeniedActionIsRecordedAndNotRetried(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const author = "deny-author-conv"
	const owner = "deny-owner-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveConvWithTitle(owner, "owner")
	f.HaveMember("alpha", author)
	ownerAgent, _, err := db.EnsureAgentForConv(owner, "test")
	require.NoError(t, err)
	rule := &db.TriggerRule{Name: "denied-message", Enabled: true, OwnerAgent: ownerAgent, ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourcePROpened, DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage, Message: &db.TriggerMessageAction{Target: "pr.author_agent", BodyTemplate: "review {{pr.url}}"}}}}
	ruleID, err := db.InsertTriggerRule(rule)
	require.NoError(t, err)
	authorAgent, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(authorAgent, "https://github.com/o/r/pull/9", "ready", "open")
	require.NoError(t, err)
	now := time.Now().Add(time.Second)
	agentd.RunTriggerTickForTest(now)
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Actions, 1)
	assert.Equal(t, "permission_denied", rows[0].Actions[0].Outcome)
	agentd.RunTriggerTickForTest(now.Add(time.Minute))
	rows, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestTriggerSpawnUsesProfileTemplatesProvenanceAndLiveBound(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	const author = "spawn-author-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("alpha", author)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	rule := &db.TriggerRule{Name: "spawn-reviewer", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftExclude, Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn,
			Spawn: &db.TriggerSpawnAction{Profile: "reviewer", NameTemplate: "review-pr-{{pr.number}}",
				InstructionTemplate: "Review {{pr.url}} on {{pr.branch}} for {{group}}.", MaxLiveWorkers: 1}}}}
	ruleID, err := db.InsertTriggerRule(rule)
	require.NoError(t, err)
	authorAgent, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPRDetails(authorAgent, "https://github.com/o/r/pull/77", "ready", "open", "topic", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().Add(time.Second))
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Actions, 1)
	assert.Equal(t, "spawned", rows[0].Actions[0].Outcome)
	spawned := rows[0].Actions[0].SpawnedAgent
	require.NotEmpty(t, spawned)
	loop, err := db.RuleSpawnedAgent(ruleID, spawned)
	require.NoError(t, err)
	assert.True(t, loop, "worker provenance is durable for loop protection")
	conv, err := db.CurrentConvForAgent(spawned)
	require.NoError(t, err)
	msgs, err := db.ListAgentMessagesForConv(conv, 20)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[0].Body, "pull/77 on topic for alpha")

	_, err = db.UpsertAgentPRDetails(authorAgent, "https://github.com/o/r/pull/78", "ready", "open", "topic-2", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().Add(2 * time.Second))
	rows, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "max_live_workers", rows[0].Actions[0].Outcome)
}
