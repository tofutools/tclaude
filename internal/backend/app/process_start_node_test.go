package app_test

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

type lostStartAdmission struct{ app.Store }

func (s lostStartAdmission) CreateGraphWorkRun(ctx context.Context, run model.WorkRun, windows []model.DecisionWindow) (app.WorkRunRecord, bool, error) {
	record, repeated, err := s.Store.CreateGraphWorkRun(ctx, run, windows)
	if err == nil {
		err = errors.New("lost admission response")
	}
	return record, repeated, err
}

func startNodeGraph() model.WorkGraph {
	return model.WorkGraph{CompilerVersion: "1", EntryNodeID: "start", Nodes: []model.WorkNode{
		{ID: "start", Kind: model.WorkNodeStart, Name: "Begin", Description: "Literal <start>", Doc: "Keep this identity"},
		{ID: "end", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "start", To: "end"}}}
}

func TestProcessStartNodeRecoversCommittedAdmission(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "lost response"}[lost], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "start.sqlite")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			service := app.New(store, providers.NewRegistry())
			if lost {
				service = app.New(lostStartAdmission{store}, providers.NewRegistry())
			}
			graph := startNodeGraph()
			_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
			if lost {
				require.ErrorContains(t, err, "lost admission response")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			defer store.Close()
			service = app.New(store, providers.NewRegistry())
			_, err = service.ReconcilePendingWork(ctx)
			require.NoError(t, err)
			result, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
			require.NoError(t, err)
			require.Equal(t, model.WorkControlSettled, result.Run.ControlState)
			require.Equal(t, model.WorkOutcomeVerified, result.Run.Outcome)
			require.Len(t, result.Run.NodeAttempts, 2)
			require.Equal(t, "Keep this identity", result.Run.Graph.Nodes[0].Doc)
			for _, attempt := range result.Run.NodeAttempts {
				require.Nil(t, attempt.Performer)
				require.Equal(t, model.NodeAttemptSucceeded, attempt.State)
			}
		})
	}
}

func TestProcessStartNodeRejectsAmbiguousRouting(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "validation.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	for _, mutate := range []func(*model.WorkGraph){
		func(g *model.WorkGraph) { g.Edges = nil },
		func(g *model.WorkGraph) {
			g.Nodes = append(g.Nodes, model.WorkNode{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true, Prompt: "Work"}}})
			g.EntryNodeID = "task"
			g.Edges = append(g.Edges, model.WorkEdge{From: "task", To: "start"})
		},
		func(g *model.WorkGraph) { g.Edges[0].Verdict = "other" },
		func(g *model.WorkGraph) { g.Nodes[0].Retry.MaxAttempts = 2 },
		func(g *model.WorkGraph) {
			g.Nodes[0].Performer = &model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true, Prompt: "wrong"}}
		},
	} {
		graph := startNodeGraph()
		draft := app.DefinitionDraft{ID: "start", RevisionID: "revision", Name: "Start", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
		_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.NoError(t, err)
		mutate(&draft.Process.Graph)
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
