package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestArchivedConfigurationRetainsPinsAndExactLifecycleRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	p := newFakeProvider()
	service := testService(store, p)
	operator := model.OperatorPrincipal()
	save := app.SaveConfigurationProfileRequest{Context: effect(operator, "save"), ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "fake", Model: "fixture", Effort: "high", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}
	profile, err := service.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	worker, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", ConfigurationProfile: &profile.Revision.Ref})
	require.NoError(t, err)
	archive := app.SetConfigurationProfileArchivedRequest{Context: effect(operator, "archive"), ID: profile.Profile.ID, ExpectedRevision: 1, Archived: true}
	denied := archive
	denied.Context.Principal = model.Principal{Kind: model.PrincipalExecution, ExecutionID: "untrusted"}
	_, err = service.SetConfigurationProfileArchived(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	archived, err := service.SetConfigurationProfileArchived(ctx, archive)
	require.NoError(t, err)
	require.True(t, archived.Archived)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "new", Name: "New", ConfigurationProfile: &profile.Revision.Ref})
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(operator, "default"), Global: &profile.Revision.Ref})
	require.ErrorIs(t, err, app.ErrConflict)
	save.Context.RequestID = "edit"
	save.RevisionID = "two"
	save.ExpectedRevision = archived.Revision
	_, err = service.SaveConfigurationProfile(ctx, save)
	require.ErrorIs(t, err, app.ErrConflict)
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: worker.Agent.ID, ExpectedRevision: worker.Agent.Revision}}})
	require.NoError(t, err)
	require.Equal(t, "high", launched.Execution.Spec.Effort)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = testService(store, p)
	retained, err := service.GetConfigurationProfile(ctx, operator, profile.Revision.Ref)
	require.NoError(t, err)
	require.True(t, retained.Profile.Archived)
	require.Equal(t, profile.Revision, retained.Revision)
	restore := archive
	restore.Context.RequestID = "restore"
	restore.ExpectedRevision = archived.Revision
	restore.Archived = false
	restored, err := service.SetConfigurationProfileArchived(ctx, restore)
	require.NoError(t, err)
	require.False(t, restored.Archived)
	replay, err := service.SetConfigurationProfileArchived(ctx, archive)
	require.NoError(t, err)
	require.Equal(t, archived, replay)
	current, err := service.GetConfigurationProfile(ctx, operator, profile.Revision.Ref)
	require.NoError(t, err)
	require.False(t, current.Profile.Archived)
	altered := archive
	altered.Archived = false
	_, err = service.SetConfigurationProfileArchived(ctx, altered)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "restored", Name: "Restored", ConfigurationProfile: &profile.Revision.Ref})
	require.NoError(t, err)
}

func TestConfigurationArchiveRechecksSelectionAndDefaultRaces(t *testing.T) {
	for _, scenario := range []string{"create", "defaults", "selected_default"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			service := testService(store, newFakeProvider())
			operator := model.OperatorPrincipal()
			profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(operator, "save"), ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "fake", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}})
			require.NoError(t, err)
			archive := app.SetConfigurationProfileArchivedRequest{Context: effect(operator, "archive"), ID: profile.Profile.ID, ExpectedRevision: 1, Archived: true}
			defaults := app.SaveConfigurationDefaultsRequest{Context: effect(operator, "default"), Global: &profile.Revision.Ref}
			if scenario == "selected_default" {
				_, err = service.SaveConfigurationDefaults(ctx, defaults)
				require.NoError(t, err)
				_, err = service.SetConfigurationProfileArchived(ctx, archive)
				require.ErrorIs(t, err, app.ErrConflict)
				return
			}
			race := &profileArchiveRaceStore{Store: store, before: func() { _, e := service.SetConfigurationProfileArchived(ctx, archive); require.NoError(t, e) }}
			raced := testService(race, newFakeProvider())
			if scenario == "create" {
				_, err = raced.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "new", Name: "New", ConfigurationProfile: &profile.Revision.Ref})
			} else {
				_, err = raced.SaveConfigurationDefaults(ctx, defaults)
			}
			require.ErrorIs(t, err, app.ErrConflict)
		})
	}
}

type profileArchiveRaceStore struct {
	app.Store
	before func()
}

func (s *profileArchiveRaceStore) CreateAgent(ctx context.Context, a model.Agent) error {
	s.before()
	return s.Store.CreateAgent(ctx, a)
}
func (s *profileArchiveRaceStore) SaveConfigurationDefaults(ctx context.Context, w app.ConfigurationDefaultsWrite) (model.ConfigurationDefaults, error) {
	s.before()
	return s.Store.SaveConfigurationDefaults(ctx, w)
}
