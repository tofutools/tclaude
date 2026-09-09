package migration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestImportClaudeDirectoryTrustPreservesProfileAndAgentBirthPolicy(t *testing.T) {
	for _, mode := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[mode], func(t *testing.T) {
			ctx := context.Background()
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, strings.ReplaceAll(`
 ALTER TABLE spawn_profiles ADD COLUMN harness TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN working_directory TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN approval TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN sandbox TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,harness,model,working_directory,approval,trust_dir,sandbox)
 VALUES('7','Claude native','[]','[]','[]','claude','fixture','/tmp','manual',FAST,'unconfined');
 `, "FAST", map[bool]string{true: "1", false: "0"}[mode]))
			alterFixture(t, bundle, strings.ReplaceAll(`UPDATE agents SET initial_spawn_config='{"harness":"claude","model":"fixture","cwd":"/tmp","approval":"manual","sandbox":"unconfined","trust_dir":BOOL}';`, "BOOL", map[bool]string{true: "true", false: "false"}[mode]))
			destination := filepath.Join(t.TempDir(), "target.sqlite")
			_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			// Reopening verifies the typed durable projection, rather than only source JSON.
			store, err := backendsqlite.Open(destination)
			require.NoError(t, err)
			service := app.New(store, providers.NewRegistry())
			op := model.OperatorPrincipal()
			profiles, err := service.ListConfigurationProfiles(ctx, op)
			require.NoError(t, err)
			require.Len(t, profiles, 1)
			profile, err := service.GetConfigurationProfile(ctx, op, model.ConfigurationProfileRef{ProfileID: profiles[0].ID})
			require.NoError(t, err)
			require.Equal(t, mode, profile.Revision.Desired.TrustDirectory)
			snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: op})
			require.NoError(t, err)
			require.Len(t, snapshot.Agents, 1)
			require.Equal(t, "claude", snapshot.Agents[0].Desired.Harness)
			require.Equal(t, mode, snapshot.Agents[0].Desired.TrustDirectory)
			require.Empty(t, snapshot.Executions)
			require.NoError(t, store.Close())
			repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			require.True(t, repeated.Repeated)
			store, err = backendsqlite.Open(destination)
			require.NoError(t, err)
			defer store.Close()
			service = app.New(store, providers.NewRegistry())
			created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "from_imported_profile", Name: "From imported profile", ConfigurationProfile: &profile.Revision.Ref})
			require.NoError(t, err)
			require.Equal(t, mode, created.Agent.Desired.TrustDirectory)
		})
	}
}

func TestImportDirectoryTrustRequiresSourceColumn(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, "ALTER TABLE spawn_profiles DROP COLUMN trust_dir")
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.Error(t, err)
	require.NoFileExists(t, destination)
}

func TestImportPartialDirectoryTrustRetainsExplicitOff(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,trust_dir) VALUES('7','Trust off','[]','[]','[]',0);`)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	profiles, err := store.ConfigurationProfiles(context.Background())
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	saved, err := store.ConfigurationProfile(context.Background(), profiles[0].ID, "")
	require.NoError(t, err)
	require.NotNil(t, saved.Revision.Options)
	require.Nil(t, saved.Revision.Options.Harness)
	require.NotNil(t, saved.Revision.Options.TrustDirectory)
	require.False(t, *saved.Revision.Options.TrustDirectory)
}
