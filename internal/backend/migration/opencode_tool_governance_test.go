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

func TestImportOpenCodeToolGovernancePreservesProfileAndAgentBirthPolicy(t *testing.T) {
	for _, mode := range []model.ToolGovernance{model.ToolGovernanceAllow, model.ToolGovernanceAsk, model.ToolGovernanceDeny} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, strings.ReplaceAll(`
 ALTER TABLE spawn_profiles ADD COLUMN harness TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN working_directory TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN approval TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN tools TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN sandbox TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,harness,model,working_directory,approval,tools,sandbox)
 VALUES('7','OpenCode native','[]','[]','[]','opencode','fixture','/tmp','deny','TOOLS','unconfined');
 UPDATE agents SET initial_spawn_config='{"harness":"opencode","model":"fixture","working_directory":"/tmp","approval":"deny","tools":"TOOLS","sandbox":"unconfined"}';
 `, "TOOLS", string(mode)))
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
			require.Equal(t, mode, profile.Revision.Desired.ToolGovernance)
			snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: op})
			require.NoError(t, err)
			require.Len(t, snapshot.Agents, 1)
			require.Equal(t, "opencode", snapshot.Agents[0].Desired.Harness)
			require.Equal(t, mode, snapshot.Agents[0].Desired.ToolGovernance)
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
			require.Equal(t, mode, created.Agent.Desired.ToolGovernance)
		})
	}
}

func TestImportOpenCodeResolvedToolGovernanceOverridesBirthAndRetainsNamedProfile(t *testing.T) {
	for _, mode := range []string{"allow", "ask", "deny"} {
		t.Run(mode, func(t *testing.T) {
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, strings.ReplaceAll(`INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs) VALUES('7','Selected','[]','[]','[]'); UPDATE agents SET initial_spawn_config='{"harness":"opencode","model":"fixture","cwd":"/tmp","approval":"deny","sandbox":"unconfined","profile":"Selected"}',relaunch_profile='{"version":1,"tools":"TOOLS"}';`, "TOOLS", mode))
			path := filepath.Join(t.TempDir(), "target.db")
			_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			store, err := backendsqlite.Open(path)
			require.NoError(t, err)
			defer store.Close()
			service := app.New(store, providers.NewRegistry())
			snapshot, err := service.Snapshot(context.Background(), app.SnapshotRequest{Principal: model.OperatorPrincipal()})
			require.NoError(t, err)
			require.Len(t, snapshot.Agents, 1)
			require.Equal(t, model.ToolGovernance(mode), snapshot.Agents[0].Desired.ToolGovernance)
			require.NotNil(t, snapshot.Agents[0].ConfigurationProfile)
		})
	}
}

func TestImportRefusesUnsupportedResolvedToolGovernanceVersion(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `UPDATE agents SET initial_spawn_config='{"harness":"opencode","cwd":"/tmp"}',relaunch_profile='{"version":2,"tools":"allow"}';`)
	path := filepath.Join(t.TempDir(), "target.db")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
	require.ErrorContains(t, err, "unsupported relaunch profile version")
	require.NoFileExists(t, path)
}
