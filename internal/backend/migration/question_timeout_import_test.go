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
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestImportClaudeAskUserQuestionTimeoutPreservesProfileAndAgentBirthPolicy(t *testing.T) {
	for _, mode := range []string{"5m", "inherit", "never", ""} {
		t.Run("window_"+mode, func(t *testing.T) {
			ctx := context.Background()
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, strings.ReplaceAll(`
 ALTER TABLE spawn_profiles ADD COLUMN harness TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN working_directory TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN approval TEXT;
 ALTER TABLE spawn_profiles ADD COLUMN sandbox TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,harness,model,working_directory,approval,ask_user_question_timeout,sandbox)
 VALUES('7','Claude native','[]','[]','[]','claude','fixture','/tmp','manual',FAST,'unconfined');
 `, "FAST", "'"+mode+"'"))
			alterFixture(t, bundle, strings.ReplaceAll(`UPDATE agents SET initial_spawn_config='{"harness":"claude","model":"fixture","cwd":"/tmp","approval":"manual","sandbox":"unconfined","ask_user_question_timeout":BOOL}';`, "BOOL", `"`+mode+`"`))
			destination := filepath.Join(t.TempDir(), "target.sqlite")
			_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			// Reopening verifies the typed durable projection, rather than only source JSON.
			store, err := backendsqlite.Open(destination)
			require.NoError(t, err)
			service := app.New(store, providers.NewRegistry(&claude.Provider{}))
			op := model.OperatorPrincipal()
			profiles, err := service.ListConfigurationProfiles(ctx, op)
			require.NoError(t, err)
			require.Len(t, profiles, 1)
			profile, err := service.GetConfigurationProfile(ctx, op, model.ConfigurationProfileRef{ProfileID: profiles[0].ID})
			require.NoError(t, err)
			require.Equal(t, model.AskUserQuestionTimeout(mode), profile.Revision.Desired.AskUserQuestionTimeout)
			snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: op})
			require.NoError(t, err)
			require.Len(t, snapshot.Agents, 1)
			require.Equal(t, "claude", snapshot.Agents[0].Desired.Harness)
			require.Equal(t, model.AskUserQuestionTimeout(mode), snapshot.Agents[0].Desired.AskUserQuestionTimeout)
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
			require.Equal(t, model.AskUserQuestionTimeout(mode), created.Agent.Desired.AskUserQuestionTimeout)
		})
	}
}

func TestImportAskUserQuestionTimeoutRequiresSourceColumn(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, "ALTER TABLE spawn_profiles DROP COLUMN ask_user_question_timeout")
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.Error(t, err)
	require.NoFileExists(t, destination)
}

func TestImportAskUserQuestionTimeoutResolvedChoiceOverridesBirth(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `UPDATE agents SET initial_spawn_config='{"harness":"claude","model":"fixture","cwd":"/tmp","approval":"manual","sandbox":"unconfined","ask_user_question_timeout":"5m"}',relaunch_profile='{"version":1,"ask_user_question_timeout":"inherit"}';`)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	snapshot, err := app.New(store, providers.NewRegistry()).Snapshot(context.Background(), app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, model.AskUserQuestionTimeout("inherit"), snapshot.Agents[0].Desired.AskUserQuestionTimeout)
}

func TestImportPartialAskUserQuestionTimeoutPreservesDefaultInheritance(t *testing.T) {
	for _, mode := range []string{"", "inherit"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			ctx := context.Background()
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, `INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,ask_user_question_timeout) VALUES('7','Native question timeout','[]','[]','[]','`+mode+`');`)
			destination := filepath.Join(t.TempDir(), "target.sqlite")
			_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			store, err := backendsqlite.Open(destination)
			require.NoError(t, err)
			defer store.Close()
			profiles, err := store.ConfigurationProfiles(ctx)
			require.NoError(t, err)
			require.Len(t, profiles, 1)
			saved, err := store.ConfigurationProfile(ctx, profiles[0].ID, "")
			require.NoError(t, err)
			require.NotNil(t, saved.Revision.Options)
			require.Nil(t, saved.Revision.Options.Harness)
			if mode == "" {
				require.Nil(t, saved.Revision.Options.AskUserQuestionTimeout, "v1 empty column is omitted intent")
			} else {
				require.NotNil(t, saved.Revision.Options.AskUserQuestionTimeout)
				require.Equal(t, model.AskUserQuestionTimeout("inherit"), *saved.Revision.Options.AskUserQuestionTimeout)
			}
			service := app.New(store, providers.NewRegistry(&claude.Provider{}))
			op := model.OperatorPrincipal()
			global, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{
				Context: app.RequestContext{Principal: op, RequestID: "global-profile"},
				ID:      "global", RevisionID: "global-v1", Name: "Global defaults",
				Desired: model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalManual, Sandbox: model.SandboxUnconfined, AskUserQuestionTimeout: "5m"},
			})
			require.NoError(t, err)
			_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: app.RequestContext{Principal: op, RequestID: "defaults"}, Global: &global.Revision.Ref})
			require.NoError(t, err)
			created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "from-partial", Name: "From partial", ConfigurationProfile: &saved.Revision.Ref})
			require.NoError(t, err)
			want := model.AskUserQuestionTimeout("5m")
			if mode == "inherit" {
				want = "inherit"
			}
			require.Equal(t, want, created.Agent.Desired.AskUserQuestionTimeout)
			require.NoError(t, store.Close())
			reopened, err := backendsqlite.Open(destination)
			require.NoError(t, err)
			defer reopened.Close()
			retained, err := reopened.Agent(ctx, created.Agent.ID)
			require.NoError(t, err)
			require.Equal(t, want, retained.Desired.AskUserQuestionTimeout)
		})
	}
}
