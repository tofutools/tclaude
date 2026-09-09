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

func TestImportedLaunchEnvironmentPreservesLiteralValuesWithoutEffects(t *testing.T) {
	ctx := context.Background()
	bundle := profileMetadataBundle(t)
	alterFixture(t, bundle, `
 UPDATE spawn_profiles SET environment_json='[{"name":"PROFILE_VALUE","value":"literal $HOME"}]';
 ALTER TABLE agent_groups ADD COLUMN environment_json TEXT;
 UPDATE agent_groups SET environment_json='[{"name":"GROUP_VALUE","value":"group=value"}]';
 UPDATE agents SET initial_spawn_config='{"harness":"codex","model":"fixture","environment":[{"name":"AGENT_VALUE","value":"agent=value"}]}';
 `)
	path := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	store, err := db.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	require.Equal(t, model.Environment{"AGENT_VALUE": "agent=value"}, snapshot.Agents[0].Desired.Environment)
	profiles, err := service.ListConfigurationProfiles(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	saved, err := service.GetConfigurationProfile(ctx, model.OperatorPrincipal(), model.ConfigurationProfileRef{ProfileID: profiles[0].ID})
	require.NoError(t, err)
	require.NotNil(t, saved.Revision.Options)
	require.Equal(t, model.Environment{"PROFILE_VALUE": "literal $HOME"}, saved.Revision.Options.Environment)
	defaults, err := service.GetGroupConfiguration(ctx, model.OperatorPrincipal(), snapshot.Groups[0].ID)
	require.NoError(t, err)
	require.Equal(t, model.Environment{"GROUP_VALUE": "group=value"}, defaults.Environment)
	authority, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Empty(t, authority.Grants)
	require.NoError(t, store.Close())
	retry, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	require.True(t, retry.Repeated)
}

func TestImportedReservedEnvironmentRetainsEvidenceInsteadOfActivation(t *testing.T) {
	ctx := context.Background()
	bundle := profileMetadataBundle(t)
	alterFixture(t, bundle, `UPDATE spawn_profiles SET environment_json='[{"name":"TCLAUDE_BACKEND_SOCKET","value":"spoof"},{"name":"NORMAL","value":"retained"}]';`)
	path := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	store, err := db.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	profiles, err := service.ListConfigurationProfiles(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	saved, err := service.GetConfigurationProfile(ctx, model.OperatorPrincipal(), model.ConfigurationProfileRef{ProfileID: profiles[0].ID})
	require.NoError(t, err)
	require.NotNil(t, saved.Revision.Options)
	require.Empty(t, saved.Revision.Options.Environment)
	report, err := store.ImportReport(ctx)
	require.NoError(t, err)
	found := false
	for _, d := range report.Diagnostics {
		if d.Code == "launch_environment_retained_unmapped" && d.SourceTable == "spawn_profiles" {
			found = true
		}
	}
	require.True(t, found)
}
