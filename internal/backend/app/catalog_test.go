package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestConfigurationProfileRetainsSelectedRevisionAcrossEditAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	req := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_one"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "claude", Model: "model_one", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}
	first, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	next := req
	next.Context.RequestID = "save_two"
	next.RevisionID = "two"
	next.ExpectedRevision = first.Profile.Revision
	next.Desired.Model = "model_two"
	second, err := service.SaveConfigurationProfile(ctx, next)
	require.NoError(t, err)
	require.Equal(t, model.Revision(2), second.Profile.Revision)
	// The old request resolves before the now-stale expected revision; it cannot rewind the catalog.
	retry, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first, retry)
	changedRetry := req
	changedRetry.Name = "different"
	_, err = service.SaveConfigurationProfile(ctx, changedRetry)
	require.ErrorIs(t, err, app.ErrConflict)
	stale := next
	stale.Context.RequestID = "stale"
	stale.RevisionID = "three"
	_, err = service.SaveConfigurationProfile(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	duplicateRevision := next
	duplicateRevision.Context.RequestID = "duplicate_revision"
	duplicateRevision.ExpectedRevision = 2
	_, err = service.SaveConfigurationProfile(ctx, duplicateRevision)
	require.Error(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	selected, err := service.GetConfigurationProfile(ctx, operator, first.Revision.Ref)
	require.NoError(t, err)
	require.Equal(t, "model_one", selected.Revision.Desired.Model)
	current, err := service.GetConfigurationProfile(ctx, operator, model.ConfigurationProfileRef{ProfileID: req.ID})
	require.NoError(t, err)
	require.Equal(t, "model_two", current.Revision.Desired.Model)
	wrong := first.Revision.Ref
	wrong.ContentHash = "wrong"
	_, err = service.GetConfigurationProfile(ctx, operator, wrong)
	require.ErrorIs(t, err, app.ErrConflict)
	profiles, err := service.ListConfigurationProfiles(ctx, operator)
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	require.Equal(t, second.Profile, profiles[0])
	denied := req
	denied.Context.Principal = model.AgentPrincipal("worker")
	_, err = service.SaveConfigurationProfile(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}

func TestSelectedProfileIsFrozenOnAgentAndExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := newFakeProvider()
	service := testService(store, provider)
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "fake", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	req := app.SaveConfigurationProfileRequest{Context: effect(operator, "profile_first"), ID: "profile", RevisionID: "first", Name: "Worker", Desired: desired}
	profile, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	mixed := app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: desired, ConfigurationProfile: &profile.Revision.Ref}
	_, err = service.CreateAgent(ctx, mixed)
	require.ErrorIs(t, err, app.ErrInvalid)
	mixed.Desired = model.DesiredConfiguration{}
	worker, err := service.CreateAgent(ctx, mixed)
	require.NoError(t, err)
	require.Equal(t, desired, worker.Agent.Desired)
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_selected"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: worker.Agent.ID, ExpectedRevision: worker.Agent.Revision}}})
	require.NoError(t, err)
	require.Equal(t, &profile.Revision.Ref, launched.Execution.Spec.ConfigurationProfile)
	req.Context.RequestID = "profile_second"
	req.RevisionID = "second"
	req.ExpectedRevision = 1
	req.Desired.Model = "second"
	newer, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	current, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	updated, err := service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: operator, ID: current.ID, ExpectedRevision: current.Revision, Name: current.Name, ConfigurationProfile: &newer.Revision.Ref})
	require.NoError(t, err)
	require.Equal(t, "second", updated.Agent.Desired.Model)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	durableAgent, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, &newer.Revision.Ref, durableAgent.ConfigurationProfile)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, "first", execution.Spec.Model)
	require.Equal(t, &profile.Revision.Ref, execution.Spec.ConfigurationProfile)
}
