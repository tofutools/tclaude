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
