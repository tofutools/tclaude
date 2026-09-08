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

func sandboxImportFixture(t *testing.T) Bundle {
	t.Helper()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `
 ALTER TABLE sandbox_profiles ADD COLUMN created_at TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN updated_at TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN network_access TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN network_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN unix_sockets_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN filesystem_spellings_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN agent_directories_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN tmpfs_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN resource_limits_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN pre_launch_json TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN filesystem_root TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN harness_config TEXT;
 ALTER TABLE sandbox_profiles ADD COLUMN darwin_allow_mach_register TEXT;
 INSERT INTO sandbox_profiles(id,name,filesystem_json,environment_json,includes_json,network_access) VALUES('1','Parent','[]','[{"name":"LITERAL","value":"$(not executed)"}]','[]','none');
 INSERT INTO sandbox_profiles(id,name,filesystem_json,environment_json,includes_json,network_json,tmpfs_json,resource_limits_json,pre_launch_json) VALUES('2','Child','[{"path":"/retained/file","mount_path":"/guest/file","access":"read","kind":"file"}]','[]','["Parent"]','{"baseline":"deny","packs":["net-anthropic"]}','[{"path":"/scratch","size":"1MiB","size_bytes":1048576}]','{"memory":"2MiB","memory_bytes":2097152,"cpu":0.25}','[{"name":"setup","script":"exit 91","exports":["PATH"]}]');
 `)
	return bundle
}

func TestImportedSandboxProfilesRemainAvailableWithClosureAndRetry(t *testing.T) {
	ctx := context.Background()
	bundle := sandboxImportFixture(t)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	result, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.Equal(t, int64(2), result.Receipt.Counts["sandbox_profiles"])
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	active, err := service.ListSandboxProfiles(ctx, model.OperatorPrincipal(), false)
	require.NoError(t, err)
	require.Len(t, active, 2)
	profiles, err := service.ListSandboxProfiles(ctx, model.OperatorPrincipal(), true)
	require.NoError(t, err)
	require.Len(t, profiles, 2)
	var child app.SandboxProfileResult
	for _, profile := range profiles {
		require.False(t, profile.Archived)
		require.True(t, profile.Imported)
		if profile.Name == "Child" {
			child, err = service.GetSandboxProfile(ctx, model.OperatorPrincipal(), profile.ID)
			require.NoError(t, err)
		}
	}
	require.Equal(t, "0.25", child.Revision.Policy.Resources.CPU)
	require.Equal(t, "exit 91", child.Revision.Policy.PreLaunch[0].Script)
	require.Equal(t, "file", child.Revision.Policy.Filesystem[0].ExpectedKind)
	closure, err := service.InspectSandboxClosure(ctx, model.OperatorPrincipal(), child.Revision.Ref)
	require.NoError(t, err)
	require.Len(t, closure.Entries, 2)
	require.Equal(t, "$(not executed)", closure.Entries[0].Policy.Environment["LITERAL"])
	require.Equal(t, "closed", closure.Entries[0].Policy.UnixSockets.Mode)
	require.Equal(t, model.SandboxNetworkDeny, closure.Entries[0].Policy.Network.Baseline)
	require.Empty(t, child.Revision.Author.Kind)

	report, err := store.ImportReport(ctx)
	require.NoError(t, err)
	for _, record := range report.Records {
		if record.SourceTable == "sandbox_profiles" {
			require.Equal(t, "sandbox_profile_available_without_execution", record.ReasonCode)
		}
	}
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	// Public lifecycle filtering is driven by scalar indexes as well as JSON;
	// exact retry must catch a changed index even if the document stays active.
	db, err := sql.Open("sqlite", destination)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sandbox_profiles SET archived=1 WHERE id=?`, child.Profile.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "not the exact completed import")
}

func TestImportedSandboxUnsupportedPolicyRetainsWholeDependentGraph(t *testing.T) {
	ctx := context.Background()
	bundle := sandboxImportFixture(t)
	alterFixture(t, bundle, `UPDATE sandbox_profiles SET filesystem_spellings_json='{"version":1,"rules":[{"resolved_path":"/canonical","spellings":["/alias"]}]}' WHERE id='1';`)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	profiles, err := store.ListSandboxProfiles(ctx, true)
	require.NoError(t, err)
	require.Empty(t, profiles)
	records, err := store.ImportedSourceRecords(ctx, "sandbox_profiles")
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Contains(t, string(records[0].Payload), "/alias")
	report, err := store.ImportReport(ctx)
	require.NoError(t, err)
	count := 0
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == "sandbox_profile_retained_unmapped" {
			count++
		}
	}
	require.Equal(t, 2, count)
	require.NoError(t, store.Close())
	retry, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, retry.Repeated)
}

func TestSandboxConversionDoesNotRewritePriorEvidenceOnlyImport(t *testing.T) {
	ctx := context.Background()
	bundle := sandboxImportFixture(t)
	inspection, err := Inspect(ctx, bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	prior, err := Translate(inspection, plan, nil, TranslationOptions{})
	require.NoError(t, err)
	prior.SandboxProfiles = nil
	delete(prior.Receipt.Counts, "sandbox_profiles")
	for i := range prior.SourceRecords {
		record := &prior.SourceRecords[i]
		if record.SourceTable == "sandbox_profiles" {
			record.Conversion = string(ConversionPending)
			record.ReasonCode = "preserved_inactive_pending_validation"
		}
	}
	prior.Receipt.SemanticSHA256, err = semanticDigest(prior)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "prior.sqlite")
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	require.NoError(t, store.ApplyImport(ctx, prior))
	require.NoError(t, store.Close())
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "not the exact completed import")
	store, err = backendsqlite.Open(destination)
	require.NoError(t, err)
	profiles, err := store.ListSandboxProfiles(ctx, true)
	require.NoError(t, err)
	require.Empty(t, profiles)
	receipt, err := store.ImportReceipt(ctx)
	require.NoError(t, err)
	require.Equal(t, prior.Receipt.SemanticSHA256, receipt.SemanticSHA256)
	require.NoError(t, store.Close())
}

func TestSandboxImportStoreRefusesInvalidProvenanceWithoutPartialPublication(t *testing.T) {
	ctx := context.Background()
	bundle := sandboxImportFixture(t)
	inspection, err := Inspect(ctx, bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	batch, err := Translate(inspection, plan, nil, TranslationOptions{})
	require.NoError(t, err)
	batch.SandboxProfiles[0].Profile.Imported = false
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	require.ErrorIs(t, store.ApplyImport(ctx, batch), app.ErrInvalid)
	profiles, err := store.ListSandboxProfiles(ctx, true)
	require.NoError(t, err)
	require.Empty(t, profiles)
	_, err = store.ImportReceipt(ctx)
	require.Error(t, err, "a refused first import must not expose a receipt")
}

func TestImportedSandboxProfileCanBeEditedDirectly(t *testing.T) {
	ctx := context.Background()
	bundle := sandboxImportFixture(t)
	destination := filepath.Join(t.TempDir(), "backend.db")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	profiles, err := service.ListSandboxProfiles(ctx, operator, true)
	require.NoError(t, err)
	require.NotEmpty(t, profiles)
	profile := profiles[0]
	edited, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "edit"}, ID: profile.ID, ExpectedRevision: profile.Revision, Name: "Updated imported profile", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "updated"}}})
	require.NoError(t, err)
	require.Equal(t, profile.ID, edited.Profile.ID)
	require.Equal(t, "updated", edited.Revision.Policy.Environment["VALUE"])
}
