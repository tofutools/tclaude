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

func TestConfigurationTransferAtomicCASArchiveAndRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	existing, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "original"}, ID: "existing", RevisionID: "original", Name: "Existing", Desired: desired})
	require.NoError(t, err)
	bundle := app.ConfigurationBundle{Format: app.ConfigurationBundleFormat, Version: 1, Profiles: []app.ConfigurationBundleEntry{{Key: "a", Name: "First", Desired: desired, Startup: &model.ProfileStartup{Context: "Retained", InitialMessage: "Start here"}, Archived: true, Disabled: true, DisabledReason: "Maintenance", Aliases: []string{"reviewer"}}, {Key: "b", Name: "Second", Desired: desired}}}
	req := app.ImportConfigurationsRequest{Context: app.RequestContext{Principal: operator, RequestID: "batch"}, Bundle: bundle, Selections: []app.ConfigurationImportSelection{{Key: "a", ID: "copy", RevisionID: "copy_one", Name: "Renamed"}, {Key: "b", ID: "existing", RevisionID: "update", ExpectedRevision: 2, Name: "Updated"}}}
	_, err = service.ImportConfigurations(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.GetConfigurationProfile(ctx, operator, model.ConfigurationProfileRef{ProfileID: "copy"})
	require.ErrorIs(t, err, app.ErrNotFound)
	req.Selections[1].ExpectedRevision = 1
	result, err := service.ImportConfigurations(ctx, req)
	require.NoError(t, err)
	require.Len(t, result.Profiles, 2)
	require.True(t, result.Profiles[0].Profile.Archived)
	require.Equal(t, []string{"reviewer"}, result.Profiles[0].Profile.Aliases)
	require.True(t, result.Profiles[0].Profile.Disabled)
	require.Equal(t, "Maintenance", result.Profiles[0].Profile.DisabledReason)
	require.Equal(t, "Start here", result.Profiles[0].Revision.Startup.InitialMessage)
	old, err := service.GetConfigurationProfile(ctx, operator, existing.Revision.Ref)
	require.NoError(t, err)
	require.Equal(t, existing.Revision, old.Revision)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	replay, err := service.ImportConfigurations(ctx, req)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, result.Profiles, replay.Profiles)
	changed := req
	changed.Selections = append([]app.ConfigurationImportSelection(nil), req.Selections...)
	changed.Selections[0].Name = "Changed"
	_, err = service.ImportConfigurations(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	denied := req
	denied.Context.Principal = model.AgentPrincipal("caller")
	_, err = service.ImportConfigurations(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	// Default selections prevent a batch from archiving that profile, including rollback of earlier entries.
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: app.RequestContext{Principal: operator, RequestID: "default"}, Global: &result.Profiles[1].Revision.Ref})
	require.NoError(t, err)
	blocked := req
	blocked.Context.RequestID = "blocked"
	blocked.Bundle.Profiles = append([]app.ConfigurationBundleEntry(nil), req.Bundle.Profiles...)
	blocked.Bundle.Profiles[1].Archived = true
	blocked.Selections = append([]app.ConfigurationImportSelection(nil), req.Selections...)
	blocked.Selections[0].ID = "another"
	blocked.Selections[0].RevisionID = "another"
	blocked.Selections[1].ExpectedRevision = 2
	blocked.Selections[1].RevisionID = "blocked"
	_, err = service.ImportConfigurations(ctx, blocked)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.GetConfigurationProfile(ctx, operator, model.ConfigurationProfileRef{ProfileID: "another"})
	require.ErrorIs(t, err, app.ErrNotFound)
	malformed := bundle
	malformed.Profiles = append([]app.ConfigurationBundleEntry(nil), bundle.Profiles...)
	malformed.Profiles[1].Key = "a"
	_, err = service.InspectConfigurationBundle(ctx, operator, malformed)
	require.ErrorIs(t, err, app.ErrInvalid)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = service.ImportConfigurations(cancelled, blocked)
	require.Error(t, err)
}
