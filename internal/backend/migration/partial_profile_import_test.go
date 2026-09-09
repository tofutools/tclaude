package migration

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestImportPartialProfilePreservesOmissionsAndLaunchContext(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
 INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,model,auto_review)
 VALUES('7','Portable','[]','[]','[]','portable-model',0);`)
	destination := filepath.Join(t.TempDir(), "target.sqlite")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry(&claude.Provider{}))
	op := model.OperatorPrincipal()
	profiles, err := service.ListConfigurationProfiles(ctx, op)
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	saved, err := service.GetConfigurationProfile(ctx, op, model.ConfigurationProfileRef{ProfileID: profiles[0].ID})
	require.NoError(t, err)
	require.NotNil(t, saved.Revision.Options)
	require.Nil(t, saved.Revision.Options.Harness)
	require.Nil(t, saved.Revision.Options.WorkingDirectory)
	require.Nil(t, saved.Revision.Options.Approval)
	require.Nil(t, saved.Revision.Options.FastMode)
	require.NotNil(t, saved.Revision.Options.AutoReview)
	require.False(t, *saved.Revision.Options.AutoReview)
	require.True(t, saved.Revision.Desired.Equal(model.DesiredConfiguration{}))
	require.NoError(t, store.Close())
	repeat, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeat.Repeated)
	store, err = backendsqlite.Open(destination)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(&claude.Provider{}))
	cwd := t.TempDir()
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "portable_agent", Name: "Portable", ConfigurationProfile: &saved.Revision.Ref, ConfigurationOverrides: &model.ConfigurationOptions{WorkingDirectory: &cwd}})
	require.NoError(t, err)
	require.Equal(t, cwd, agent.Agent.Desired.WorkingDirectory)
	require.Equal(t, "portable-model", agent.Agent.Desired.Model)
	require.NoError(t, store.Close())
}
