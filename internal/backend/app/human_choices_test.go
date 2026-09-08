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

func TestHumanTaskChoicesUseAdmittedMappingAcrossRestartAndRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "choices.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages = nil
	graph.Nodes[0].Performer.Human.Choices = []string{"cancel", "waive"}
	graph.Nodes[0].Performer.Human.ChoiceOutcomes = map[string]string{"cancel": "fail", "waive": "pass"}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "choices", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	var window model.DecisionWindow
	for _, d := range run.Decisions {
		window = d
	}
	require.Equal(t, []string{"cancel", "waive"}, window.PermittedAnswers)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	request := app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "fail"}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, Answer: "cancel", Reason: "needs changes"}
	_, err = service.SubmitDecision(ctx, request)
	require.NoError(t, err)
	_, err = service.SubmitDecision(ctx, request)
	require.NoError(t, err)
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "choices"})
	require.NoError(t, err)
	require.NotEqual(t, model.WorkRunCancelled, run.Run.State)
	var retry model.DecisionWindow
	for _, d := range run.Decisions {
		if d.State == model.DecisionOpen {
			retry = d
		}
	}
	require.NotEmpty(t, retry.ID)
	require.Equal(t, window.PermittedAnswers, retry.PermittedAnswers)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "unknown"}, DecisionID: retry.ID, ExpectedWindowRevision: retry.Revision, Answer: "complete", Reason: "not offered"})
	require.Error(t, err)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "pass"}, DecisionID: retry.ID, ExpectedWindowRevision: retry.Revision, Answer: "waive", Reason: "verified"})
	require.NoError(t, err)
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "choices"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, run.Run.State)
	require.Equal(t, model.WorkOutcomeVerified, run.Run.Outcome)
}

func TestHumanTaskChoicesRejectIncompleteAndAmbiguousMappings(t *testing.T) {
	for name, human := range map[string]model.HumanPerformer{
		"newline":         {Choices: []string{"approve\nwith note"}, ChoiceOutcomes: map[string]string{"approve\nwith note": "pass"}},
		"carriage-return": {Choices: []string{"approve\rwith note"}, ChoiceOutcomes: map[string]string{"approve\rwith note": "pass"}},
		"missing":         {Choices: []string{"Accept"}},
		"extra":           {ChoiceOutcomes: map[string]string{"Accept": "pass"}},
		"case":            {Choices: []string{"Accept", "ACCEPT"}, ChoiceOutcomes: map[string]string{"Accept": "pass", "ACCEPT": "fail"}},
		"space":           {Choices: []string{" Accept"}, ChoiceOutcomes: map[string]string{" Accept": "pass"}},
		"outcome":         {Choices: []string{"Accept"}, ChoiceOutcomes: map[string]string{"Accept": "waive"}},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "choices.sqlite"))
			require.NoError(t, err)
			defer store.Close()
			graph := stagedHumanGraph()
			human.Operator = true
			human.Prompt = "Review"
			graph.Nodes[0].Stages.Review.Performer.Human = &human
			service := app.New(store, providers.NewRegistry())
			_, err = service.ValidateDefinition(context.Background(), app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: app.DefinitionDraft{ID: "choices", Name: "Choices", Kind: model.DefinitionProcess, Process: &model.ProcessDefinition{Graph: graph}}})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
}
