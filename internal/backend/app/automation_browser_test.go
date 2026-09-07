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

func TestAutomationEnableReceiptReopenAndCurrentAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	now := time.Now().UTC()
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	rule, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "revision", Name: "Schedule", Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Hour}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "Hello", AgentIDs: []model.AgentID{"recipient"}}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, Overlap: model.OverlapForbid, MaxActive: 1, ExpiresAfter: time.Hour, Deadline: time.Hour}})
	require.NoError(t, err)
	request := app.SetAutomationEnabledRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "enable"}, ID: rule.Rule.ID, ExpectedRevision: rule.Rule.Revision, Enabled: true}
	enabled, err := service.SetAutomationEnabled(ctx, request)
	require.NoError(t, err)
	require.True(t, enabled.Enabled)
	require.Equal(t, rule.Rule.HeadRevisionID, enabled.HeadRevisionID)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	repeated, err := service.SetAutomationEnabled(ctx, request)
	require.NoError(t, err)
	require.Equal(t, enabled, repeated)
	mutated := request
	mutated.Enabled = false
	_, err = service.SetAutomationEnabled(ctx, mutated)
	require.ErrorIs(t, err, app.ErrConflict)
	stale := request
	stale.Context.RequestID = "stale"
	stale.Enabled = false
	_, err = service.SetAutomationEnabled(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	disable := stale
	disable.Context.RequestID = "disable"
	disable.ExpectedRevision = enabled.Revision
	disabled, err := service.SetAutomationEnabled(ctx, disable)
	require.NoError(t, err)
	require.False(t, disabled.Enabled)
	request.Context.RequestID = "reenable"
	request.ExpectedRevision = disabled.Revision
	again, err := service.SetAutomationEnabled(ctx, request)
	require.NoError(t, err)
	require.True(t, again.Enabled)
	require.Equal(t, rule.Rule.HeadRevisionID, again.HeadRevisionID)
	denied := request
	denied.Context = app.RequestContext{Principal: model.Principal{Kind: model.PrincipalAgent, AgentID: "unknown"}, RequestID: "denied"}
	denied.ExpectedRevision = again.Revision
	denied.Enabled = false
	_, err = service.SetAutomationEnabled(ctx, denied)
	require.Error(t, err)
	current, err := service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: model.OperatorPrincipal(), ID: "rule"})
	require.NoError(t, err)
	require.True(t, current.Rule.Enabled)
}

func TestRecurringWorkGetsAnOccurrenceDeadlineInsteadOfExpiredTemplateTime(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer store.Close()
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "done", Nodes: []model.WorkNode{{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}}
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "revision", Name: "Recurring work", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionStartWork}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule"}}, ExpiresAt: now.Add(24 * time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Second)}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickCoalesce, OfflineDelivery: model.OfflineQueue, Overlap: model.OverlapAllow, MaxActive: 2, ExpiresAfter: time.Hour, Deadline: 10 * time.Minute}})
	require.NoError(t, err)
	for range 2 {
		now = now.Add(5 * time.Minute)
		for range 3 {
			_, err = service.ReconcilePendingWork(ctx)
			require.NoError(t, err)
		}
	}
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "rule"})
	require.NoError(t, err)
	require.Len(t, occurrences, 2)
	for _, record := range occurrences {
		require.NotEmpty(t, record.Occurrence.WorkRunID)
		work, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: record.Occurrence.WorkRunID})
		require.NoError(t, err)
		require.Equal(t, record.Occurrence.EligibleAt.Add(10*time.Minute), work.Run.Deadline)
	}
}

func TestAutomationPinnedDefinitionReadDoesNotFollowHeadOrCrossIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	draft := app.DefinitionDraft{ID: "process", RevisionID: "first", Name: "Original", Source: "pinned process", Kind: model.DefinitionProcess, SchemaVersion: 1, Process: &model.ProcessDefinition{Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: "done", Nodes: []model.WorkNode{{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}}}}
	first, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "first"}, Draft: draft})
	require.NoError(t, err)
	draft.RevisionID = "second"
	draft.Name = "New name"
	_, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "second"}, Draft: draft, ExpectedRevision: first.Definition.Revision})
	require.NoError(t, err)
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "process", RevisionID: "first"})
	require.NoError(t, err)
	require.Equal(t, first.Revision, read.Revision)
	draft.ID, draft.RevisionID = "other", "other_revision"
	_, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "other"}, Draft: draft})
	require.NoError(t, err)
	_, err = service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "other", RevisionID: "first"})
	require.ErrorIs(t, err, app.ErrNotFound)
}
