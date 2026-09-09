package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func profileMetadataBundle(t *testing.T) Bundle {
	t.Helper()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `
 ALTER TABLE spawn_profiles ADD COLUMN role TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN descr TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN effort TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN agent_name TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN initial_message TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN startup_context TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN disabled TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN disabled_reason TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,effort,agent_name,initial_message,startup_context,disabled)
 VALUES('7','safe','[]','[]','[]','high','Imported writer','saved-private-brief','saved-private-context','1');
 UPDATE spawn_profiles SET disabled_reason='Provider maintenance',role='writer',descr='Drafts documentation' WHERE id='7';
 ALTER TABLE agents ADD COLUMN effort TEXT;
 UPDATE agents SET effort='low',initial_spawn_config='{"effort":"medium","harness":"codex","model":"fixture"}';
 `)
	return bundle
}

func TestImportProfileMetadataRoundTripWithoutActivation(t *testing.T) {
	ctx := context.Background()
	bundle := profileMetadataBundle(t)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	result, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.Equal(t, 2, result.Receipt.ImporterFormatVersion)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	profiles, err := service.ListConfigurationProfiles(ctx, model.OperatorPrincipal())
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	require.True(t, profiles[0].Disabled)
	require.False(t, profiles[0].Archived)
	require.Equal(t, "Provider maintenance", profiles[0].DisabledReason)
	selected, err := service.GetConfigurationProfile(ctx, model.OperatorPrincipal(), model.ConfigurationProfileRef{ProfileID: profiles[0].ID})
	require.NoError(t, err)
	require.Equal(t, "high", selected.Revision.Desired.Effort)
	require.Equal(t, &model.ProfileStartup{Role: "writer", Description: "Drafts documentation", AgentName: "Imported writer", Context: "saved-private-context", InitialMessage: "saved-private-brief"}, selected.Revision.Startup)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, "medium", snapshot.Agents[0].Desired.Effort)
	require.Empty(t, snapshot.Executions)
	state, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Empty(t, state.Grants)
	require.Empty(t, state.Assignments)
	records, err := store.ImportedSourceRecords(ctx, "spawn_profiles")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Contains(t, string(records[0].Payload), "saved-private-brief")
	report, err := store.ImportReport(ctx)
	require.NoError(t, err)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "saved-private")
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}

func TestImportProfileMetadataRetryRefusesChangedTargetAndOldFormat(t *testing.T) {
	for _, mutation := range []string{"UPDATE configuration_profile_revisions SET record=json_set(record,'$.Startup.InitialMessage','changed')", "UPDATE agents SET effort='max'", "UPDATE configuration_profiles SET record=json_set(record,'$.Disabled',json('false'))", "UPDATE import_receipts SET importer_format_version=1"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			bundle := profileMetadataBundle(t)
			destination := filepath.Join(t.TempDir(), "target.sqlite")
			_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			db, err := sql.Open("sqlite", destination)
			require.NoError(t, err)
			_, err = db.Exec(mutation)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
			require.Error(t, err)
		})
	}
}

func TestImportInvalidStartupRetainsOriginalAndRedactsDiagnostic(t *testing.T) {
	ctx := context.Background()
	bundle := profileMetadataBundle(t)
	alterFixture(t, bundle, `UPDATE spawn_profiles SET initial_message='invalid-secret'||char(0),disabled='unknown',effort='INVALID-secret'; UPDATE agents SET effort='high',initial_spawn_config='{"effort":""}';`)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	profiles, err := store.ConfigurationProfiles(ctx)
	require.NoError(t, err)
	require.True(t, profiles[0].Archived)
	selected, err := store.ConfigurationProfile(ctx, profiles[0].ID, "")
	require.NoError(t, err)
	require.Nil(t, selected.Revision.Startup)
	require.Equal(t, "INVALID-secret", selected.Revision.Desired.Effort)
	snapshot, err := store.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snapshot.Agents[0].Desired.Effort, "blank pinned effort must not inherit mutable row value")
	report, err := store.ImportReport(ctx)
	require.NoError(t, err)
	codes := map[string]bool{}
	for _, d := range report.Diagnostics {
		codes[d.Code] = true
	}
	require.True(t, codes["profile_startup_retained_unmapped"])
	require.True(t, codes["profile_disabled_unrecognized"])
	require.True(t, codes["requested_effort_requires_review"])
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret")
	records, err := store.ImportedSourceRecords(ctx, "spawn_profiles")
	require.NoError(t, err)
	require.Contains(t, string(records[0].Payload), "invalid-secret")
}

func TestImportInvalidUTF8StartupRefusesPublication(t *testing.T) {
	bundle := profileMetadataBundle(t)
	alterFixture(t, bundle, `UPDATE spawn_profiles SET initial_message=CAST(X'FF' AS TEXT);`)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "source text is not valid UTF-8")
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
}
