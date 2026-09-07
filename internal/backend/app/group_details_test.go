package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestGroupDetailsRequireOperatorCASAndSurviveOrdinaryUpdates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	details := model.GroupDetails{Description: "<script>plain text</script>\nDescription", Mission: "Mission", LinkURL: "https://example.org/task", LinkLabel: "Task"}
	in := app.SetGroupDetailsRequest{Principal: op, ID: "group", ExpectedRevision: 1, Details: details}
	denied := in
	denied.Principal = model.AgentPrincipal("caller")
	_, err = svc.SetGroupDetails(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	changed, err := svc.SetGroupDetails(ctx, in)
	require.NoError(t, err)
	require.Equal(t, &details, changed.Details)
	_, err = svc.SetGroupDetails(ctx, in)
	require.ErrorIs(t, err, app.ErrConflict)
	updated, err := svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "group", Name: "Renamed", ExpectedRevision: changed.Revision})
	require.NoError(t, err)
	require.Equal(t, &details, updated.Group.Details)
	require.NoError(t, store.Close())
	store, err = db.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	snapshot, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Equal(t, &details, snapshot.Groups[0].Details)
	require.Empty(t, snapshot.Executions)
	require.Empty(t, snapshot.Groups[0].Members)
	for _, url := range []string{"javascript:alert(1)", "file:///tmp/a", "https://user:secret@example.org/", "https://example.org/\npath"} {
		invalid := in
		invalid.ExpectedRevision = updated.Group.Revision
		invalid.Details.LinkURL = url
		_, err = svc.SetGroupDetails(ctx, invalid)
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	cleared, err := svc.SetGroupDetails(ctx, app.SetGroupDetailsRequest{Principal: op, ID: "group", ExpectedRevision: updated.Group.Revision})
	require.NoError(t, err)
	require.Nil(t, cleared.Details)
	require.Equal(t, "Renamed", cleared.Name)
}
