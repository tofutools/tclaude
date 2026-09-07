package app_test

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestDefinitionEditorLayoutSurvivesRevisionAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	draft := app.DefinitionDraft{ID: "process", RevisionID: "first", Name: "Example", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "preserved source and comments", Process: &model.ProcessDefinition{Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: "done", Nodes: []model.WorkNode{{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}}}, EditorLayout: &model.DefinitionEditorLayout{Nodes: map[model.WorkNodeID]model.EditorPosition{"done": {X: 123, Y: 456}}}}
	request := app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save-first"}, Draft: draft}
	first, err := service.SaveDefinition(ctx, request)
	require.NoError(t, err)
	_, err = service.SaveDefinition(ctx, request)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "process"})
	require.NoError(t, err)
	require.Equal(t, first.Revision.EditorLayout, read.Revision.EditorLayout)
	require.Equal(t, draft.Source, read.Revision.Source)
	draft.RevisionID = "second"
	draft.EditorLayout = &model.DefinitionEditorLayout{Nodes: map[model.WorkNodeID]model.EditorPosition{"done": {X: 321, Y: 456}}}
	second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save-second"}, Draft: draft, ExpectedRevision: first.Definition.Revision})
	require.NoError(t, err)
	require.NotEqual(t, first.Revision.ContentHash, second.Revision.ContentHash)
	original, err := store.DefinitionRevision(ctx, "first")
	require.NoError(t, err)
	require.Equal(t, float64(123), original.EditorLayout.Nodes["done"].X)
	_, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "stale"}, Draft: draft, ExpectedRevision: first.Definition.Revision})
	require.ErrorIs(t, err, app.ErrConflict)
	for name, layout := range map[string]*model.DefinitionEditorLayout{
		"unknown-node": {Nodes: map[model.WorkNodeID]model.EditorPosition{"missing": {}}},
		"unbounded":    {Nodes: map[model.WorkNodeID]model.EditorPosition{"done": {X: 1e7}}},
		"nan":          {Nodes: map[model.WorkNodeID]model.EditorPosition{"done": {Y: math.NaN()}}},
	} {
		t.Run(name, func(t *testing.T) {
			draft.EditorLayout = layout
			_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
}
