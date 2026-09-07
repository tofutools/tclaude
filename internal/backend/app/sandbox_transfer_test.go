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

func TestSandboxTransferRemapsExactClosureAtomicallyAndRetriesAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	parent, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "parent"}, ID: "parent", Name: "Parent", Policy: model.SandboxPolicy{Environment: model.Environment{"LITERAL": "$(not-executed)"}}})
	require.NoError(t, err)
	child, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "child"}, ID: "child", Name: "Child", Policy: model.SandboxPolicy{Includes: []model.SandboxProfileRef{parent.Revision.Ref}}})
	require.NoError(t, err)
	bundle, err := service.ExportSandboxBundle(ctx, operator, child.Revision.Ref)
	require.NoError(t, err)
	req := app.ImportSandboxProfilesRequest{Context: app.RequestContext{Principal: operator, RequestID: "import"}, Bundle: bundle, Selections: []app.SandboxImportSelection{
		{Source: parent.Revision.Ref, ID: "parent_copy", RevisionID: "parent_copy_revision", Name: "Independent parent"},
		{Source: child.Revision.Ref, ID: "child", RevisionID: "child_copy_revision", Name: "Independent child"},
	}}
	_, err = service.ImportSandboxProfiles(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.GetSandboxProfile(ctx, operator, "parent_copy")
	require.ErrorIs(t, err, app.ErrNotFound)
	req.Selections[1].ID = "child_copy"
	saved, err := service.ImportSandboxProfiles(ctx, req)
	require.NoError(t, err)
	require.False(t, saved.Repeated)
	closure, err := service.InspectSandboxClosure(ctx, operator, saved.Root)
	require.NoError(t, err)
	require.Len(t, closure.Entries, 2)
	require.Equal(t, "$(not-executed)", closure.Entries[0].Policy.Environment["LITERAL"])
	require.Equal(t, model.SandboxProfileID("parent_copy"), closure.Entries[1].Policy.Includes[0].ProfileID)
	// Source request is not mutated by remapping.
	require.Equal(t, parent.Revision.Ref, req.Bundle.Entries[1].Policy.Includes[0])
	_, err = service.SetSandboxProfileArchived(ctx, app.SetSandboxProfileArchivedRequest{Context: app.RequestContext{Principal: operator, RequestID: "archive-copy"}, ID: "child_copy", ExpectedRevision: 1, Archived: true})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	repeated, err := service.ImportSandboxProfiles(ctx, req)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	repeated.Repeated = false
	require.Equal(t, saved, repeated)
	req.Selections[1].Name = "Changed intent"
	_, err = service.ImportSandboxProfiles(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	req.Context.Principal = model.AgentPrincipal("untrusted")
	_, err = store.ImportSandboxProfiles(ctx, req, time.Now())
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = service.ExportSandboxBundle(ctx, req.Context.Principal, child.Revision.Ref)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}
