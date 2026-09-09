package migration

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestImportedGroupDefaultPinsSourceIdentityAndRequiresExplicitCreation(t *testing.T) {
	for _, disabled := range []int{0, 1} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			ctx := context.Background()
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, fmt.Sprintf(`
 ALTER TABLE spawn_profiles ADD COLUMN harness TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN working_directory TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN approval TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN sandbox TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN initial_message TEXT;

 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,harness,model,working_directory,approval,sandbox,initial_message,disabled)
 VALUES('7','Selected','[]','[]','[]','codex','selected-model','/tmp','supervised','workspace_write','Selected brief',%d),
 ('8','7','[]','[]','[]','codex','wrong-model','/tmp','supervised','workspace_write','Wrong brief',0);
 UPDATE agent_groups SET default_profile_id='7';`, disabled))
			path := filepath.Join(t.TempDir(), "db")
			result, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			inspection, err := Inspect(ctx, bundle)
			require.NoError(t, err)
			plan, err := Plan(inspection)
			require.NoError(t, err)
			groupID := model.GroupID(findIdentity(t, plan, "agent_groups", "1").TargetID)
			profileID := model.ConfigurationProfileID(findIdentity(t, plan, "spawn_profiles", "7").TargetID)
			store, err := db.Open(path)
			require.NoError(t, err)
			svc := app.New(store, providers.NewRegistry())
			op := model.OperatorPrincipal()
			defaults, err := svc.GetGroupConfiguration(ctx, op, groupID)
			require.NoError(t, err)
			require.NotNil(t, defaults.Profile)
			require.Equal(t, profileID, defaults.Profile.ProfileID)
			require.Equal(t, model.Revision(1), defaults.Revision)
			profile, err := svc.GetConfigurationProfile(ctx, op, *defaults.Profile)
			require.NoError(t, err)
			require.Equal(t, "selected-model", profile.Revision.Desired.Model)
			require.Equal(t, "Selected brief", profile.Revision.Startup.InitialMessage)
			snap, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
			require.NoError(t, err)
			require.Len(t, snap.Agents, 1)
			require.Empty(t, snap.Executions)
			authority, err := store.AuthorityState(ctx)
			require.NoError(t, err)
			require.Empty(t, authority.Grants)
			require.Empty(t, authority.Assignments)
			require.NoError(t, store.Close())
			repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			require.True(t, repeated.Repeated)
			require.Equal(t, result.Receipt.SemanticSHA256, repeated.Receipt.SemanticSHA256)
			require.Equal(t, int64(1), result.Receipt.Counts["group_configurations"])
			// Mutating the pinned imported row is detected before accepting an exact retry.
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			_, err = raw.Exec(`UPDATE group_configurations SET content_hash='changed'`)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
			require.Error(t, err)
			raw, err = sql.Open("sqlite", path)
			require.NoError(t, err)
			_, err = raw.Exec(`UPDATE group_configurations SET content_hash=?`, defaults.Profile.ContentHash)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			store, err = db.Open(path)
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			svc = app.New(store, providers.NewRegistry())
			member, err := svc.CreateGroupMember(ctx, app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: op, RequestID: "explicit_member"}, GroupID: groupID, ID: "explicit_member", Name: "Explicit member", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision})
			if disabled != 0 {
				require.ErrorIs(t, err, app.ErrConflict)
				_, err = store.Agent(ctx, "explicit_member")
				require.ErrorIs(t, err, app.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, defaults.Profile, member.Agent.ConfigurationProfile)
				require.Equal(t, "selected-model", member.Agent.Desired.Model)
			}
			snap, err = svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
			require.NoError(t, err)
			require.Empty(t, snap.Executions)
		})
	}
}

func TestImportedGroupDefaultNameWithoutIDIsDiagnosedWithoutGuessing(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agent_groups ADD COLUMN default_profile TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs) VALUES('7','Selected','[]','[]','[]');
 UPDATE agent_groups SET default_profile='Selected',default_profile_id=NULL;`)
	path := filepath.Join(t.TempDir(), "db")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	inspection, err := Inspect(ctx, bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	store, err := db.Open(path)
	require.NoError(t, err)
	svc := app.New(store, providers.NewRegistry())
	defaults, err := svc.GetGroupConfiguration(ctx, model.OperatorPrincipal(), model.GroupID(findIdentity(t, plan, "agent_groups", "1").TargetID))
	require.NoError(t, err)
	require.Nil(t, defaults.Profile)
	require.Zero(t, defaults.Revision)
	report, err := store.ImportReport(ctx)
	require.NoError(t, err)
	found := false
	for _, d := range report.Diagnostics {
		if d.Code == "group_default_retained_unmapped" {
			found = true
		}
	}
	require.True(t, found)
	rows, err := store.ImportedSourceRecords(ctx, "agent_groups")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, string(rows[0].Payload), "Selected")
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}
