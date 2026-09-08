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
	alterFixture(t, bundle, `ALTER TABLE agent_group_members ADD COLUMN descr TEXT; UPDATE agent_conversations SET role='head'; UPDATE agent_group_members SET role='engineer',descr='literal <b>description</b>';`)
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
	require.Equal(t, model.AgentDisplayLabels{Role: "engineer", Description: "literal <b>description</b>"}, snapshot.Agents[0].Labels.InGroup(snapshot.Groups[0].ID))
	authority, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Empty(t, authority.Assignments)
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}

func TestImportedAgentLabelsKeepDistinctMembershipsWithoutGenerationLabels(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agent_group_members ADD COLUMN descr TEXT; UPDATE agent_conversations SET role='head';
 UPDATE agent_group_members SET role='reviewer',descr='First team';
 INSERT INTO agent_groups(id,name,owner_scopes_json) VALUES('2','second','[]');
 INSERT INTO agent_group_members(group_id,agent_id,role,descr,joined_at) VALUES('2','agt_fixture','author','Second team',2);`)
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
	labels := snapshot.Agents[0].Labels
	require.Empty(t, labels.Role, "conversation head/generation metadata is not a display role")
	require.Len(t, labels.Groups, 2)
	for _, group := range snapshot.Groups {
		expected := model.AgentDisplayLabels{Role: "reviewer", Description: "First team"}
		if group.Name == "second" {
			expected = model.AgentDisplayLabels{Role: "author", Description: "Second team"}
		}
		require.Equal(t, expected, labels.InGroup(group.ID))
	}
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}
