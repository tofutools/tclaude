package agentd_test

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestTriggerCIPollerUsesFreshTransitionsAndDurableBaseline(t *testing.T) {
	f := triggerFlow(t)
	g := f.HaveGroup("ci-watch")
	const author = "ci-trigger-author"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("ci-watch", author)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "ci-failure", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourceCIFailed,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{BodyTemplate: "CI {{event.previous_state}} -> {{event.current_state}} on {{pr.branch}} for {{pr.url}}"}}}})
	require.NoError(t, err)
	agentID, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPRDetails(agentID, "https://github.com/o/r/pull/90", "checks", "open", "ci-topic", false)
	require.NoError(t, err)
	var calls atomic.Int32
	t.Cleanup(agentd.SetPRChecksResolverForTest(func(string) (string, bool) {
		if calls.Add(1) == 1 {
			return `[{"__typename":"CheckRun","name":"build","status":"IN_PROGRESS"}]`, true
		}
		return `[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE"}]`, true
	}))
	base := time.Now().UTC().Add(time.Second)
	agentd.PollTriggerCITransitionsForTest(base)
	agentd.RunTriggerTickForTest(base)
	firings, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Empty(t, firings, "the first fresh state only establishes the durable baseline")
	agentd.PollTriggerCITransitionsForTest(base.Add(time.Second))
	agentd.RunTriggerTickForTest(base.Add(time.Second))
	firings, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	assert.Equal(t, "ok", firings[0].Outcome)
	messages, err := db.ListAgentMessagesForConv(author, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0].Body, "on ci-topic")

	// Re-reading the same fresh failing state, including after another engine
	// tick, cannot synthesize a restart edge.
	agentd.PollTriggerCITransitionsForTest(base.Add(2 * time.Second))
	agentd.RunTriggerTickForTest(base.Add(2 * time.Second))
	firings, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Len(t, firings, 1)

	require.NoError(t, config.Save(&config.Config{}))
	before := calls.Load()
	agentd.PollTriggerCITransitionsForTest(base.Add(3 * time.Second))
	assert.Equal(t, before, calls.Load(), "flag-off poller does not resolve GitHub state")
}

func TestTriggerCIPollerAppliesTerminalStateBeforeChecks(t *testing.T) {
	f := triggerFlow(t)
	f.HaveGroup("terminal-ci")
	f.HaveConvWithTitle("terminal-ci-author", "author")
	f.HaveMember("terminal-ci", "terminal-ci-author")
	agentID, err := db.AgentIDForConv("terminal-ci-author")
	require.NoError(t, err)
	pr, err := db.UpsertAgentPR(agentID, "https://github.com/o/r/pull/91", "terminal", "open")
	require.NoError(t, err)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "terminal-ci", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourceCIFailed, DraftFilter: db.TriggerDraftInclude,
		Actions: []db.TriggerAction{{Type: db.TriggerActionMessage, Message: &db.TriggerMessageAction{BodyTemplate: "fail"}}}})
	require.NoError(t, err)
	var calls atomic.Int32
	t.Cleanup(agentd.SetPRChecksStateResolverForTest(func(string) (string, string, bool) {
		if calls.Add(1) == 1 {
			return `[{"__typename":"CheckRun","name":"build","status":"IN_PROGRESS"}]`, "open", true
		}
		return `[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE"}]`, "merged", true
	}))
	base := time.Now().UTC().Add(time.Second)
	agentd.PollTriggerCITransitionsForTest(base)
	agentd.PollTriggerCITransitionsForTest(base.Add(time.Second))
	agentd.RunTriggerTickForTest(base.Add(2 * time.Second))
	firings, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Empty(t, firings, "terminal response must not create a late CI edge")
	got, err := db.GetAgentPR(pr.AgentID, pr.PRURL)
	require.NoError(t, err)
	assert.Equal(t, "merged", got.State)
	agentd.PollTriggerCITransitionsForTest(base.Add(3 * time.Second))
	assert.Equal(t, int32(2), calls.Load(), "terminal PR leaves the watched set")
}

func TestTriggerCIPollerRebaselinesOnlyNewlyReenabledScope(t *testing.T) {
	f := triggerFlow(t)
	groupA := f.HaveGroup("ci-a")
	groupB := f.HaveGroup("ci-b")
	f.HaveConvWithTitle("ci-author-a", "a")
	f.HaveConvWithTitle("ci-author-b", "b")
	f.HaveMember("ci-a", "ci-author-a")
	f.HaveMember("ci-b", "ci-author-b")
	makeRule := func(name string, groupID int64) int64 {
		id, err := db.InsertTriggerRule(&db.TriggerRule{Name: name, Enabled: true, OperatorAuthored: true,
			ScopeKind: db.TriggerScopeGroup, GroupID: groupID, Source: db.TriggerSourceCIFailed,
			DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
				Message: &db.TriggerMessageAction{BodyTemplate: "failed"}}}})
		require.NoError(t, err)
		return id
	}
	ruleA, ruleB := makeRule("ci-a-fail", groupA.ID), makeRule("ci-b-fail", groupB.ID)
	agentA, err := db.AgentIDForConv("ci-author-a")
	require.NoError(t, err)
	agentB, err := db.AgentIDForConv("ci-author-b")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentA, "https://github.com/o/r/pull/101", "a", "open")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentB, "https://github.com/o/r/pull/102", "b", "open")
	require.NoError(t, err)
	states := map[string]string{"101": "pending", "102": "pending"}
	t.Cleanup(agentd.SetPRChecksResolverForTest(func(rawURL string) (string, bool) {
		state := states[rawURL[len(rawURL)-3:]]
		if state == "failing" {
			return `[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE"}]`, true
		}
		return `[{"__typename":"CheckRun","name":"build","status":"IN_PROGRESS"}]`, true
	}))
	base := time.Now().UTC().Add(time.Second)
	agentd.PollTriggerCITransitionsForTest(base)
	rule, err := db.GetTriggerRule(ruleB)
	require.NoError(t, err)
	require.NoError(t, db.SetTriggerRuleEnabled(ruleB, rule.RowVersion, false))
	agentd.PollTriggerCITransitionsForTest(base.Add(time.Second)) // observes B leaving the watched set
	states["101"], states["102"] = "failing", "failing"
	agentd.PollTriggerCITransitionsForTest(base.Add(2 * time.Second))
	rule, err = db.GetTriggerRule(ruleB)
	require.NoError(t, err)
	require.NoError(t, db.SetTriggerRuleEnabled(ruleB, rule.RowVersion, true))
	agentd.PollTriggerCITransitionsForTest(base.Add(3 * time.Second))
	agentd.RunTriggerTickForTest(base.Add(4 * time.Second))
	firingsA, err := db.ListTriggerFirings(ruleA, 10)
	require.NoError(t, err)
	firingsB, err := db.ListTriggerFirings(ruleB, 10)
	require.NoError(t, err)
	require.Len(t, firingsA, 1, "continuously watched scope keeps its transition")
	assert.Empty(t, firingsB, "re-enabled scope baselines current state instead of replaying the gap")
}

func TestTriggerCIPollerDedupesCanonicalPRIdentityAcrossPresentations(t *testing.T) {
	f := triggerFlow(t)
	f.HaveGroup("ci-dup")
	f.HaveConvWithTitle("ci-dup-a", "a")
	f.HaveConvWithTitle("ci-dup-b", "b")
	f.HaveMember("ci-dup", "ci-dup-a")
	f.HaveMember("ci-dup", "ci-dup-b")
	agentA, err := db.AgentIDForConv("ci-dup-a")
	require.NoError(t, err)
	agentB, err := db.AgentIDForConv("ci-dup-b")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentA, "https://github.com/Owner/Repo/pull/103", "a", "open")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentB, "https://github.com/owner/repo/pull/103/files", "b", "open")
	require.NoError(t, err)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "ci-dedup", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourceCIFailed, DraftFilter: db.TriggerDraftInclude,
		Actions: []db.TriggerAction{{Type: db.TriggerActionMessage, Message: &db.TriggerMessageAction{BodyTemplate: "fail"}}}})
	require.NoError(t, err)
	var calls atomic.Int32
	t.Cleanup(agentd.SetPRChecksResolverForTest(func(string) (string, bool) {
		if calls.Add(1) == 1 {
			return `[{"__typename":"CheckRun","name":"build","status":"IN_PROGRESS"}]`, true
		}
		return `[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE"}]`, true
	}))
	base := time.Now().UTC().Add(time.Second)
	agentd.PollTriggerCITransitionsForTest(base)
	agentd.PollTriggerCITransitionsForTest(base.Add(time.Second))
	agentd.RunTriggerTickForTest(base.Add(2 * time.Second))
	firings, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	assert.Equal(t, int32(2), calls.Load(), "one fresh resolver call per canonical identity and cadence")

	// A daemon restart with the feature and rule still enabled initializes the
	// watched set without clearing the durable identity owner's baseline.
	agentd.InitializeTriggerCIWatchStateForTest()
	agentd.PollTriggerCITransitionsForTest(base.Add(3 * time.Second))
	agentd.RunTriggerTickForTest(base.Add(4 * time.Second))
	firings, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Len(t, firings, 1)
	assert.Equal(t, int32(3), calls.Load())
}

func TestDashboardSnapshotDynamicallyGatesTriggers(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	handler := agentd.BuildDashboardHandlerForTest()
	assert.False(t, fetchDashSnapshot(t, handler).TriggersEnabled)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}))
	assert.True(t, fetchDashSnapshot(t, handler).TriggersEnabled)
}

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

func triggerFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	agentd.ResetTriggerCIWatchStateForTest()
	t.Cleanup(agentd.ResetTriggerCIWatchStateForTest)
	f := newFlow(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}))
	return f
}

func TestTriggersFeatureFlagGatesRoutesAndEngine(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	dash := agentd.BuildDashboardHandlerForTest()

	for _, route := range []struct {
		handler http.Handler
		req     *http.Request
	}{
		{dash, dashReq(t, http.MethodGet, "/api/triggers", nil)},
		{f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/triggers", nil), "flag-author")},
	} {
		rec := testharness.Serve(route.handler, route.req)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"triggers_disabled"`)
		assert.Contains(t, rec.Body.String(), config.TriggersDisabledMessage)
	}
	readInfo := func() bool {
		rec := testharness.Serve(f.Mux, testharness.JSONRequest(t, http.MethodGet, "/v1/info", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Triggers bool `json:"triggers"`
		}
		testharness.DecodeJSON(t, rec, &body)
		return body.Triggers
	}
	assert.False(t, readInfo(), "daemon capability defaults off")

	f.HaveConvWithTitle("flag-author", "author")
	f.HaveMember("alpha", "flag-author")
	authorAgent, err := db.AgentIDForConv("flag-author")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(authorAgent, "https://github.com/o/r/pull/1", "off", "open")
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now())
	events, err := db.ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	assert.Empty(t, events, "disabled fire point and sweep are no-ops")

	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}))
	assert.True(t, readInfo(), "daemon capability reflects explicit enablement")
	rec := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/triggers", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = db.UpsertAgentPR(authorAgent, "https://github.com/o/r/pull/2", "on", "open")
	require.NoError(t, err)
	events, err = db.ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.NotEmpty(t, events, "enabled fire point persists the opening edge")
}

func TestTriggerRuntimeHotEnableStartsDormantSchedulers(t *testing.T) {
	f := newFlow(t)
	agentd.ResetTriggerCIWatchStateForTest()
	t.Cleanup(agentd.ResetTriggerCIWatchStateForTest)
	f.HaveGroup("hot-enable")
	const author = "hot-enable-author"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("hot-enable", author)
	restore := agentd.SetTriggerIntervalsForTest(10*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(restore)
	stop := make(chan struct{})
	agentd.StartTriggerRuntimeForTest(stop)
	t.Cleanup(func() { close(stop) })

	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}))
	g, err := db.GetAgentGroupByName("hot-enable")
	require.NoError(t, err)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "hot-enabled", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{BodyTemplate: "hot {{pr.url}}"}}}})
	require.NoError(t, err)
	agentID, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentID, "https://github.com/o/r/pull/2", "hot", "open")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rows, listErr := db.ListTriggerFirings(ruleID, 10)
		return listErr == nil && len(rows) == 1 && rows[0].Outcome == "ok"
	}, time.Second, 10*time.Millisecond)
}

func TestTriggerExplainRejectsUnknownSource(t *testing.T) {
	f := triggerFlow(t)
	f.HaveConvWithTitle("explain-source", "explain")
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/triggers/explain", map[string]any{"source": "ci.typo", "pr_url": "https://github.com/o/r/pull/1"}), "explain-source"))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"invalid_arg"`)
}

func TestTriggerExplainSurfacesUnknownAgentFact(t *testing.T) {
	f := triggerFlow(t)
	const conv = "explain-idle-conv"
	f.HaveConvWithTitle(conv, "idle")
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	_, err = db.InsertTriggerRule(&db.TriggerRule{Name: "idle-dwell", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourceAgentIdle, DraftFilter: db.TriggerDraftInclude,
		ForSeconds: 60, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{Target: "agent", BodyTemplate: "wake"}}}})
	require.NoError(t, err)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/triggers/explain", map[string]any{"source": db.TriggerSourceAgentIdle, "agent_id": agentID})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Results []struct {
			Outcome    string `json:"outcome"`
			FactResult string `json:"fact_result"`
			Detail     string `json:"detail"`
		} `json:"results"`
	}
	testharness.DecodeJSON(t, rec, &body)
	require.Len(t, body.Results, 1)
	assert.Equal(t, "unknown", body.Results[0].Outcome)
	assert.Equal(t, "unknown", body.Results[0].FactResult)
	assert.NotEmpty(t, body.Results[0].Detail)
}

func TestDashboardTriggerStateSourceRoundTripsDwell(t *testing.T) {
	triggerFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost, "/api/triggers", map[string]any{
		"name": "awaiting-human", "enabled": true, "scope": "global",
		"source": db.TriggerSourceAgentAwaitingInput, "draft_filter": db.TriggerDraftInclude,
		"for_seconds": 300, "actions": []map[string]any{{"type": "message", "message": map[string]any{
			"target": "agent", "body_template": "Question waiting for {{agent.id}}",
		}}},
	}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		Source     string `json:"source"`
		ForSeconds int64  `json:"for_seconds"`
		Actions    []struct {
			Message struct {
				Target string `json:"target"`
			} `json:"message"`
		} `json:"actions"`
	}
	testharness.DecodeJSON(t, rec, &created)
	assert.Equal(t, db.TriggerSourceAgentAwaitingInput, created.Source)
	assert.Equal(t, int64(300), created.ForSeconds)
	require.Len(t, created.Actions, 1)
	assert.Equal(t, "agent", created.Actions[0].Message.Target)
}

func TestDashboardTriggersCRUDContract(t *testing.T) {
	f := triggerFlow(t)
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
	var replaced struct {
		RowVersion      int64              `json:"row_version"`
		Name            string             `json:"name"`
		DebounceSeconds int64              `json:"debounce_seconds"`
		CooldownSeconds int64              `json:"cooldown_seconds"`
		Actions         []db.TriggerAction `json:"actions"`
	}
	testharness.DecodeJSON(t, rec, &replaced)
	rec = testharness.Serve(dash, dashReq(t, http.MethodPatch, fmt.Sprintf("/api/triggers/%d", created.ID), map[string]any{
		"row_version": replaced.RowVersion, "cooldown_seconds": 30,
	}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var partial struct {
		Name            string             `json:"name"`
		DebounceSeconds int64              `json:"debounce_seconds"`
		CooldownSeconds int64              `json:"cooldown_seconds"`
		Actions         []db.TriggerAction `json:"actions"`
	}
	testharness.DecodeJSON(t, rec, &partial)
	assert.Equal(t, "review-nudge-v2", partial.Name)
	assert.Equal(t, int64(10), partial.DebounceSeconds)
	assert.Equal(t, int64(30), partial.CooldownSeconds)
	assert.Len(t, partial.Actions, 1, "partial PATCH preserves omitted actions")
	rec = testharness.Serve(dash, dashReq(t, http.MethodPatch, fmt.Sprintf("/api/triggers/%d", created.ID), body))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestTriggerRestartClosesRunningFiringWithoutReplay(t *testing.T) {
	f := triggerFlow(t)
	f.HaveGroup("alpha")
	const author = "interrupted-author-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("alpha", author)
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost, "/api/triggers", triggerMessageBody("alpha", 0)))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var created struct{ ID int64 }
	testharness.DecodeJSON(t, rec, &created)
	authorAgent, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(authorAgent, "https://github.com/o/r/pull/88", "ready", "open")
	require.NoError(t, err)
	events, err := db.ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	_, inserted, err := db.InsertTriggerFiring(created.ID, 1, events[0].ID, events[0].EventRef, "running", "", time.Now())
	require.NoError(t, err)
	require.True(t, inserted)
	agentd.RunTriggerTickForTest(time.Now().Add(time.Second))
	firings, err := db.ListTriggerFirings(created.ID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	assert.Equal(t, "interrupted", firings[0].Outcome)
	assert.Empty(t, firings[0].Actions)
	events, err = db.ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	assert.Empty(t, events, "interrupted event must not be replayed")
	d, err := db.Open()
	require.NoError(t, err)
	var eventStatus string
	var processedAt any
	require.NoError(t, d.QueryRow(`SELECT status,processed_at FROM trigger_pr_events WHERE id=?`, firings[0].EventID).Scan(&eventStatus, &processedAt))
	assert.Equal(t, db.TriggerEventInterrupted, eventStatus)
	assert.NotNil(t, processedAt)
	msgs, err := db.ListAgentMessagesForConv(author, 10)
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestTriggerReadPermissionsFilterGlobalAndGroupRules(t *testing.T) {
	f := triggerFlow(t)
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
	f := triggerFlow(t)
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
	f := triggerFlow(t)
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
	f := triggerFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", Model: "sonnet"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "group-default", Harness: "claude", Model: "opus"})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("alpha", "group-default")
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
	model, ok := f.World.SpawnModel(conv)
	require.True(t, ok)
	assert.Equal(t, "sonnet", model, "trigger-selected profile must override the group's default profile")
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

func TestTriggerMaxLiveWorkersIsPerAction(t *testing.T) {
	f := triggerFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	const author = "multi-action-author-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("alpha", author)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	action := db.TriggerAction{Type: db.TriggerActionSpawn, Spawn: &db.TriggerSpawnAction{Profile: "reviewer", InstructionTemplate: "Review {{pr.url}}", MaxLiveWorkers: 1}}
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "two-reviewers", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{action, action}})
	require.NoError(t, err)
	authorAgent, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(authorAgent, "https://github.com/o/r/pull/99", "ready", "open")
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().Add(time.Second))
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Actions, 2)
	assert.Equal(t, "spawned", rows[0].Actions[0].Outcome)
	assert.Equal(t, "spawned", rows[0].Actions[1].Outcome)
}
