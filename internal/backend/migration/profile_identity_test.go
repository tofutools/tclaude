package migration

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestImportedProfileIdentityKeepsNamesSeparateFromIDs(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs) VALUES('7','Selected','[]','[]','[]'),('8','7','[]','[]','[]'); ALTER TABLE dashboard_prefs ADD COLUMN value TEXT; INSERT INTO dashboard_prefs(key,value) VALUES('tclaude.dash.default_profile_id','7'); INSERT INTO spawn_profile_aliases(profile_id,alias) VALUES('7','alias'); UPDATE agents SET relaunch_profile='alias';`)
	inspection, err := Inspect(ctx, bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	wanted := model.ConfigurationProfileID(findIdentity(t, plan, "spawn_profiles", "7").TargetID)
	path := filepath.Join(t.TempDir(), "db")
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	store, err := db.Open(path)
	require.NoError(t, err)
	defaults, err := store.ConfigurationDefaults(ctx)
	require.NoError(t, err)
	require.NotNil(t, defaults.Global)
	require.Equal(t, wanted, defaults.Global.ProfileID)
	agent, err := store.Agent(ctx, model.AgentID(findIdentity(t, plan, "agents", "agt_fixture").TargetID))
	require.NoError(t, err)
	require.NotNil(t, agent.ConfigurationProfile)
	require.Equal(t, wanted, agent.ConfigurationProfile.ProfileID)
	require.NoError(t, store.Close())
	retry, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	require.True(t, retry.Repeated)
}

func TestImportedNumericProfileNameDoesNotBecomeAnID(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs) VALUES('7','8','[]','[]','[]'),('8','Selected','[]','[]','[]'); ALTER TABLE dashboard_prefs ADD COLUMN value TEXT; INSERT INTO dashboard_prefs(key,value) VALUES('tclaude.dash.default_profile','8'); UPDATE agents SET relaunch_profile='8';`)
	inspection, err := Inspect(ctx, bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	wanted := model.ConfigurationProfileID(findIdentity(t, plan, "spawn_profiles", "7").TargetID)
	path := filepath.Join(t.TempDir(), "db")
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	defaults, err := store.ConfigurationDefaults(ctx)
	require.NoError(t, err)
	require.NotNil(t, defaults.Global)
	require.Equal(t, wanted, defaults.Global.ProfileID)
	agent, err := store.Agent(ctx, model.AgentID(findIdentity(t, plan, "agents", "agt_fixture").TargetID))
	require.NoError(t, err)
	require.NotNil(t, agent.ConfigurationProfile)
	require.Equal(t, wanted, agent.ConfigurationProfile.ProfileID)
}

func TestImportedProfileSentinelIDRetainsNamedDefault(t *testing.T) {
	for _, value := range []string{"", "0"} {
		t.Run("id_"+value, func(t *testing.T) {
			ctx := context.Background()
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, `INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs) VALUES('7','Selected','[]','[]','[]'); ALTER TABLE dashboard_prefs ADD COLUMN value TEXT; INSERT INTO dashboard_prefs(key,value) VALUES('tclaude.dash.default_profile','Selected'),('tclaude.dash.default_profile_id','`+value+`');`)
			inspection, err := Inspect(ctx, bundle)
			require.NoError(t, err)
			plan, err := Plan(inspection)
			require.NoError(t, err)
			wanted := model.ConfigurationProfileID(findIdentity(t, plan, "spawn_profiles", "7").TargetID)
			path := filepath.Join(t.TempDir(), "db")
			_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			store, err := db.Open(path)
			require.NoError(t, err)
			defaults, err := store.ConfigurationDefaults(ctx)
			require.NoError(t, err)
			require.NotNil(t, defaults.Global)
			require.Equal(t, wanted, defaults.Global.ProfileID)
			require.NoError(t, store.Close())
			retry, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			require.True(t, retry.Repeated)
		})
	}
}
