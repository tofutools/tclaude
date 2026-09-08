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

func TestProcessWaitAuthoringPreservesExternalIntentWithoutStarting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wait.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	wait := model.WaitPolicy{Duration: time.Minute, Until: "2030-09-08T10:30:00+02:00", Signal: "review <approved>"}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "wait", Nodes: []model.WorkNode{{ID: "wait", Kind: model.WorkNodeWait, Wait: &wait}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "wait", To: "done"}}}
	draft := app.DefinitionDraft{ID: "wait", RevisionID: "wait_v1", Name: "External wait", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "wait"})
	require.NoError(t, err)
	require.Equal(t, wait, *read.Revision.Process.Graph.Nodes[0].Wait)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
	wait.Until = ""
	wait.Signal = ""
	draft.RevisionID = "wait_v2"
	second, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "clear"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrUnsupported)
	ref.RevisionID = second.Revision.ID
	ref.ContentHash = second.Revision.ContentHash
	_, err = service.StartProcess(ctx, start)
	require.NoError(t, err)
	for _, invalid := range []model.WaitPolicy{{}, {Duration: -time.Second}, {Until: "artifact:approved"}, {Signal: "\x00bad"}, {Signal: "   "}, {Until: "2030-02-31T00:00:00Z"}} {
		*draft.Process.Graph.Nodes[0].Wait = invalid
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
