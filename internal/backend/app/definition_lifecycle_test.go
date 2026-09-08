package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestDefinitionArchiveRetainsPinsAndRequiresExplicitRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	draft := app.DefinitionDraft{ID: "library", Name: "Reusable", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "authored", Process: &model.ProcessDefinition{Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: "done", Nodes: []model.WorkNode{{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}}}}
	save := app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft}
	original, err := service.SaveDefinition(ctx, save)
	require.NoError(t, err)
	request := app.SetDefinitionArchivedRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "archive"}, ID: draft.ID, ExpectedRevision: original.Definition.Revision, Archived: true}
	archived, err := service.SetDefinitionArchived(ctx, request)
	require.NoError(t, err)
	require.True(t, archived.Tombstoned)
	require.Equal(t, original.Revision.ID, archived.HeadRevisionID)
	visible, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, visible)
	all, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal(), IncludeTombstoned: true})
	require.NoError(t, err)
	require.Len(t, all, 1)
	edit := save
	edit.Context.RequestID = "edit"
	edit.ExpectedRevision = archived.Revision
	edit.Draft.Name = "Must not restore"
	_, err = service.SaveDefinition(ctx, edit)
	require.ErrorIs(t, err, app.ErrConflict)
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: draft.ID, RevisionID: original.Revision.ID})
	require.NoError(t, err)
	require.Equal(t, original.Revision, read.Revision)
	pin := model.DefinitionRef{DefinitionID: draft.ID, RevisionID: original.Revision.ID, ContentHash: original.Revision.ContentHash, Kind: model.DefinitionProcess}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "pinned"}, ID: "pinned", Start: model.WorkStart{Definition: &pin, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, run.Run.State)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	repeated, err := service.SetDefinitionArchived(ctx, request)
	require.NoError(t, err)
	require.Equal(t, archived, repeated)
	changed := request
	changed.Archived = false
	_, err = service.SetDefinitionArchived(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	denied := request
	denied.Context = app.RequestContext{Principal: model.AgentPrincipal("untrusted"), RequestID: "denied"}
	_, err = service.SetDefinitionArchived(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	stale := request
	stale.Context.RequestID = "stale"
	_, err = service.SetDefinitionArchived(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	restore := request
	restore.Context.RequestID = "restore"
	restore.ExpectedRevision = archived.Revision
	restore.Archived = false
	restored, err := service.SetDefinitionArchived(ctx, restore)
	require.NoError(t, err)
	require.False(t, restored.Tombstoned)
	repeated, err = service.SetDefinitionArchived(ctx, request)
	require.NoError(t, err)
	require.Equal(t, archived, repeated, "exact retry returns receipt without re-archiving")
	current, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: draft.ID})
	require.NoError(t, err)
	require.False(t, current.Definition.Tombstoned)
	edit.ExpectedRevision = restored.Revision
	edit.Context.RequestID = "after_restore"
	_, err = service.SaveDefinition(ctx, edit)
	require.NoError(t, err)
}
