package github

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestGitHubDwellFailureRestartAndNoDuplicateWork(t *testing.T) {
	ctx := context.Background()
	var failed atomic.Bool
	source := setup(t, func(w http.ResponseWriter, r *http.Request) bool {
		if failed.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	now := testTime
	path := filepath.Join(t.TempDir(), "state.sqlite")
	var store *sqlite.Store
	var service *app.Service
	open := func() {
		var err error
		store, err = sqlite.Open(path)
		require.NoError(t, err)
		service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithAutomationFactSources(source)
	}
	open()
	defer func() { require.NoError(t, store.Close()) }()
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "review", Nodes: []model.WorkNode{{ID: "review", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "review", To: "done"}}}
	_, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "checks", RevisionID: "checks_v1", Name: "stable CI", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionStartWork}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "checks"}, {Kind: model.ResourceWorkRun}}, ExpiresAt: now.Add(24 * time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationTrigger, Trigger: &model.TriggerCondition{SourceID: source.SourceID(), Resource: request().Resource, FactKind: model.FactCICompleted, Values: []string{"succeeded"}, Dwell: 2 * time.Minute, Freshness: 3 * time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickCoalesce, OfflineDelivery: model.OfflineSkip, ExpiresAfter: time.Hour, Overlap: model.OverlapForbid, MaxActive: 1, Deadline: time.Hour, Retry: model.RetryPolicy{MaxAttempts: 1}}})
	require.NoError(t, err)
	sweep := func() { t.Helper(); _, err := service.ReconcilePendingWork(ctx); require.NoError(t, err) }
	count := func() int {
		t.Helper()
		records, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "checks"})
		require.NoError(t, err)
		return len(records)
	}
	sweep()
	require.Zero(t, count())
	now = now.Add(time.Minute)
	failed.Store(true)
	sweep()
	require.Zero(t, count())
	now = now.Add(time.Minute)
	failed.Store(false)
	sweep()
	require.Zero(t, count())
	require.NoError(t, store.Close())
	open()
	now = now.Add(time.Minute)
	sweep()
	require.Zero(t, count(), "failure must reset rather than carry old dwell")
	now = now.Add(time.Minute)
	sweep()
	require.Equal(t, 1, count())
	for range 3 {
		now = now.Add(time.Minute)
		sweep()
	}
	require.Equal(t, 1, count(), "continuous true observations cannot duplicate a work occurrence after restart")
}
