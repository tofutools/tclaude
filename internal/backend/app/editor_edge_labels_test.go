package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestEditorEdgeLabelsPersistWithoutChangingRouting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	edge := model.WorkEdge{From: "choice", To: "done", Verdict: "yes"}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "choice", Nodes: []model.WorkNode{
		{ID: "choice", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{PermittedAnswers: []string{"yes"}, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, ExpiresAfter: time.Hour}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{edge}}
	draft := app.DefinitionDraft{ID: "labels", RevisionID: "v1", Name: "Labels", Source: "test", Kind: model.DefinitionProcess, SchemaVersion: 1, Process: &model.ProcessDefinition{Graph: graph}, EditorLayout: &model.DefinitionEditorLayout{EdgeLabels: []model.EditorEdgeLabel{{Edge: edge, Pinned: false}}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "labels"})
	require.NoError(t, err)
	require.Equal(t, saved.Revision.EditorLayout, read.Revision.EditorLayout)
	require.Equal(t, graph.Edges, read.Revision.Process.Graph.Edges)
	for _, labels := range [][]model.EditorEdgeLabel{
		{{Edge: model.WorkEdge{From: "choice", To: "done", Verdict: "wrong"}, Pinned: true}},
		{{Edge: edge, Pinned: true}, {Edge: edge, Pinned: false}},
	} {
		draft.EditorLayout.EdgeLabels = labels
		_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	selection := model.ProcessSelection{Version: 1, Positions: map[model.WorkNodeID]model.EditorPosition{}, Nodes: graph.Nodes, Edges: graph.Edges, EdgeLabels: read.Revision.EditorLayout.EdgeLabels}
	raw, err := json.Marshal(selection)
	require.NoError(t, err)
	canonical, err := app.CanonicalProcessSelection(raw)
	require.NoError(t, err)
	var restored model.ProcessSelection
	require.NoError(t, json.Unmarshal(canonical, &restored))
	require.Equal(t, selection.EdgeLabels, restored.EdgeLabels)
	restored.Edges = nil
	raw, err = json.Marshal(restored)
	require.NoError(t, err)
	_, err = app.CanonicalProcessSelection(raw)
	require.ErrorIs(t, err, app.ErrInvalid)
}
