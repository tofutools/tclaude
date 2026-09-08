package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAuthoredStageRetriesAndModesPreservePinnedIntent(t *testing.T) {
	for _, target := range []string{"task", "plan", "check", "review"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "retry.sqlite")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			service := app.New(store, providers.NewRegistry())
			graph := stagedHumanGraph()
			var policy *model.RetryPolicy
			switch target {
			case "task":
				policy = &graph.Nodes[0].Retry
			case "plan":
				policy = &graph.Nodes[0].Stages.Plan.Retry
			case "check":
				policy = &graph.Nodes[0].Stages.Checks[0].Retry
			case "review":
				policy = &graph.Nodes[0].Stages.Review.Retry
			}
			*policy = model.RetryPolicy{MaxAttempts: 3, Backoff: time.Second, Retryable: []string{model.RetryableHumanRejection}, OnFail: "fresh-attempt"}
			if target == "task" || target == "plan" {
				policy.OnFail = "feedback-same-session"
			}
			draft := app.DefinitionDraft{ID: "retry", RevisionID: "retry_v1", Name: "Retry", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			service = app.New(store, providers.NewRegistry())
			read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "retry"})
			require.NoError(t, err)
			require.Equal(t, saved.Revision.Process, read.Revision.Process)
			ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
			start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}
			_, err = service.StartProcess(ctx, start)
			require.ErrorIs(t, err, app.ErrUnsupported)
			_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
			require.ErrorIs(t, err, app.ErrNotFound)
			*policy = model.RetryPolicy{}
			draft.RevisionID = "retry_v2"
			second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "clear"}, Draft: draft, ExpectedRevision: 1})
			require.NoError(t, err)
			_, err = service.StartProcess(ctx, start)
			require.ErrorIs(t, err, app.ErrUnsupported)
			ref.RevisionID = second.Revision.ID
			ref.ContentHash = second.Revision.ContentHash
			_, err = service.StartProcess(ctx, start)
			require.NoError(t, err)
		})
	}
}

func TestRetryModeValidationAndFreshAttemptExecution(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "mode.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	graph := stagedHumanGraph()
	graph.Nodes[0].Retry.OnFail = "fresh-attempt"
	draft := app.DefinitionDraft{ID: "mode", RevisionID: "mode_v1", Name: "Mode", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.NoError(t, err)
	for _, mode := range []string{"same-session", " fresh-attempt ", "bad\x00mode"} {
		graph.Nodes[0].Retry.OnFail = mode
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	graph.Nodes[0].Retry = model.RetryPolicy{OnFail: "fresh-attempt"}
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.ErrorIs(t, err, app.ErrInvalid)
}

func TestOccurrenceRetryRejectsProcessOnlyModes(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "retry.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	request := app.SaveAutomationRuleRequest{
		Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "baseline"},
		ID:      "rule", RevisionID: "baseline", Name: "Schedule",
		Owner:     model.AuthoritySubject{Kind: model.AuthorityOperator},
		Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Hour}},
		Action:    model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "Hello", AgentIDs: []model.AgentID{"recipient"}}},
		Policy:    model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, Overlap: model.OverlapForbid, MaxActive: 1, ExpiresAfter: time.Hour, Deadline: time.Hour, Retry: model.RetryPolicy{MaxAttempts: 2}},
	}
	saved, err := service.SaveAutomationRule(ctx, request)
	require.NoError(t, err)
	for _, mode := range []string{"not-a-retry-mode", "fresh-attempt", "feedback-same-session"} {
		t.Run(mode, func(t *testing.T) {
			changed := request
			changed.Context.RequestID = model.RequestID(mode)
			changed.RevisionID = model.AutomationRuleRevisionID(mode)
			changed.ExpectedRevision = saved.Rule.Revision
			changed.Policy.Retry.OnFail = mode
			_, err := service.SaveAutomationRule(ctx, changed)
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
	rules, err := service.ListAutomationRules(ctx, app.ListAutomationRulesRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, saved.Rule, rules[0])
}
