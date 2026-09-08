package migration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func assignedSandboxFixture(t *testing.T) Bundle {
	t.Helper()
	bundle := sandboxImportFixture(t)
	alterFixture(t, bundle, `ALTER TABLE sandbox_profile_global_assignment ADD COLUMN profile_id TEXT; ALTER TABLE sandbox_profile_global_assignment ADD COLUMN profile_name TEXT;
 INSERT INTO sandbox_profile_global_assignment VALUES(1,'1','stale name');
 ALTER TABLE agent_groups ADD COLUMN sandbox_profile_id TEXT;
 ALTER TABLE agent_groups ADD COLUMN sandbox_profile TEXT;
 UPDATE agent_groups SET sandbox_profile_id='2',sandbox_profile='stale child name' WHERE id='1';`)
	return bundle
}

func TestImportedSandboxDefaultsPreserveIDsNamesAndRetry(t *testing.T) {
	ctx := context.Background()
	bundle := assignedSandboxFixture(t)
	destination := filepath.Join(t.TempDir(), "backend.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defaults, err := store.SandboxDefaults(ctx)
	require.NoError(t, err)
	profiles, err := store.ListSandboxProfiles(ctx, false)
	require.NoError(t, err)
	require.Len(t, profiles, 2)
	names := map[model.SandboxProfileID]string{}
	for _, p := range profiles {
		names[p.ID] = p.Name
	}
	require.Equal(t, "Parent", names[defaults.Global])
	require.Len(t, defaults.Groups, 1)
	for _, id := range defaults.Groups {
		require.Equal(t, "Child", names[id])
	}
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	// Editing the imported profile keeps assignment identity and is immediately
	// visible to fresh closure resolution; conversion never starts a workload.
	store, err = backendsqlite.Open(destination)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	_, err = service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "edit"}, ID: defaults.Global, ExpectedRevision: 1, Name: "Renamed parent", Policy: model.SandboxPolicy{Environment: model.Environment{"LITERAL": "new"}}})
	require.NoError(t, err)
	current, err := store.SandboxDefaults(ctx)
	require.NoError(t, err)
	require.Equal(t, defaults, current)
	require.NoError(t, store.Close())
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "not the exact completed import")
}

func TestImportedSandboxAssignmentRefusesMissingOrUnconvertibleProfile(t *testing.T) {
	for _, change := range []string{
		`UPDATE sandbox_profile_global_assignment SET profile_id='missing',profile_name='Parent'`,
		`UPDATE sandbox_profiles SET filesystem_spellings_json='{"version":1,"rules":[{"resolved_path":"/canonical","spellings":["/alias"]}]}' WHERE id='1'`,
	} {
		t.Run(change, func(t *testing.T) {
			bundle := assignedSandboxFixture(t)
			alterFixture(t, bundle, change)
			destination := filepath.Join(t.TempDir(), "refused.sqlite")
			_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			require.Error(t, err)
			require.NoFileExists(t, destination)
		})
	}
}

func TestImportedSandboxAssignmentNameFallbackAndTypedVerification(t *testing.T) {
	bundle := assignedSandboxFixture(t)
	alterFixture(t, bundle, `UPDATE sandbox_profile_global_assignment SET profile_id='0',profile_name='Parent'; UPDATE agent_groups SET sandbox_profile_id=NULL,sandbox_profile='Child'`)
	destination := filepath.Join(t.TempDir(), "backend.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	db, err := sql.Open("sqlite", destination)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sandbox_defaults SET record='{"Global":"","Groups":{},"Revision":1}'`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "not the exact completed import")
}
