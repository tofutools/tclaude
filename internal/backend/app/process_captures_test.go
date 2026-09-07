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

func TestProcessCaptureAuthoringRetainsNamesAndRefusesExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "captures.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages = nil
	graph.Nodes[0].Captures = []string{"diff", "test-report"}
	draft := app.DefinitionDraft{ID: "captures", RevisionID: "captures_v1", Name: "Captured task", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "captures"})
	require.NoError(t, err)
	require.Equal(t, []string{"diff", "test-report"}, read.Revision.Process.Graph.Nodes[0].Captures)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "run"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
	// Clearing the authored names is an explicit new revision, not an implicit execution fallback.
	draft.Process.Graph.Nodes[0].Captures = nil
	draft.RevisionID = "captures_v2"
	second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "clear"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported, "original pin retains its capture declaration")
	ref.RevisionID = second.Revision.ID
	ref.ContentHash = second.Revision.ContentHash
	_, err = service.StartProcess(ctx, start)
	require.NoError(t, err)
	for _, names := range [][]string{{"duplicate", "duplicate"}, {"not valid"}, {"Uppercase"}} {
		draft.Process.Graph.Nodes[0].Captures = names
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	draft.Process.Graph.Nodes[0].Captures = nil
	draft.Process.Graph.Nodes[1].Captures = []string{"invalid-on-end"}
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.ErrorIs(t, err, app.ErrInvalid)
}
