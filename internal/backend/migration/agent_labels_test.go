package migration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestImportedAgentDisplayLabelsRemainLiteralWithoutAuthority(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agents ADD COLUMN role TEXT; ALTER TABLE agents ADD COLUMN descr TEXT; UPDATE agents SET role='engineer',descr='literal <b>description</b>';`)
	ctx := context.Background()
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := db.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	snapshot, err := app.New(store, providers.NewRegistry()).Snapshot(ctx, app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, model.AgentLabels{Role: "engineer", Description: "literal <b>description</b>"}, snapshot.Agents[0].Labels)
	authority, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Empty(t, authority.Assignments)
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}
