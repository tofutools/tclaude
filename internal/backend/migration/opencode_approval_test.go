package migration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestImportOpenCodeDenyPreservesProfileAndAgentBirthPolicy(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `
 ALTER TABLE spawn_profiles ADD COLUMN harness TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN working_directory TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN approval TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN sandbox TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,harness,model,working_directory,approval,sandbox)
 VALUES('7','OpenCode unattended','[]','[]','[]','opencode','fixture','/tmp','deny','unconfined');
 UPDATE agents SET initial_spawn_config='{"harness":"opencode","model":"fixture","working_directory":"/tmp","approval":"deny","sandbox":"unconfined"}';
 `)
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
	require.Equal(t, model.ApprovalDeny, profile.Revision.Desired.Approval)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, "opencode", snapshot.Agents[0].Desired.Harness)
	require.Equal(t, model.ApprovalDeny, snapshot.Agents[0].Desired.Approval)
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
	require.Equal(t, model.ApprovalDeny, created.Agent.Desired.Approval)
}

func TestImportedDenyIsNotAssignedToOtherProviders(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "copilot", ""} {
		t.Run(harness, func(t *testing.T) {
			desired := desiredFromRow(map[string]any{"harness": harness, "approval": "deny"})
			require.Empty(t, desired.Approval)
		})
	}
}
