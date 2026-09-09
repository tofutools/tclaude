package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestConfigurationAliasesResolveCurrentAndFenceCollisions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	aliases := []string{"reviewer", "🌟 review"}
	req := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "save"}, ID: "worker", RevisionID: "one", Name: "Primary", Aliases: &aliases, Desired: model.DesiredConfiguration{Harness: "claude", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}
	first, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	selected, err := service.ResolveConfigurationProfile(ctx, op, " reviewer ")
	require.NoError(t, err)
	require.Equal(t, first, selected)
	collision := req
	collision.Context.RequestID = "collision"
	collision.ID = "other"
	collision.RevisionID = "other"
	collision.Name = "reviewer"
	collision.Aliases = nil
	_, err = service.SaveConfigurationProfile(ctx, collision)
	require.ErrorIs(t, err, app.ErrConflict)
	req.Context.RequestID = "edit"
	req.RevisionID = "two"
	req.ExpectedRevision = 1
	req.Desired.Model = "second"
	req.Aliases = nil
	second, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, aliases, second.Profile.Aliases)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry())
	selected, err = service.ResolveConfigurationProfile(ctx, op, "🌟 review")
	require.NoError(t, err)
	require.Equal(t, "second", selected.Revision.Desired.Model)
	clear := []string{}
	req.Context.RequestID = "clear"
	req.RevisionID = "three"
	req.ExpectedRevision = 2
	req.Aliases = &clear
	cleared, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Empty(t, cleared.Profile.Aliases)
	_, err = service.ResolveConfigurationProfile(ctx, op, "reviewer")
	require.ErrorIs(t, err, app.ErrNotFound)
	replay, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, cleared, replay)
	collision.Context.RequestID = "reuse"
	_, err = service.SaveConfigurationProfile(ctx, collision)
	require.NoError(t, err)
}

func TestConfigurationAliasTransferUsesFinalNamespace(t *testing.T) {
	for _, gainFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "gain_first", false: "release_first"}[gainFirst], func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
			require.NoError(t, err)
			defer store.Close()
			service := app.New(store, providers.NewRegistry())
			op := model.OperatorPrincipal()
			desired := model.DesiredConfiguration{Harness: "claude", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
			for _, name := range []string{"a", "b", "outside"} {
				aliases := []string{}
				if name == "a" {
					aliases = []string{"moveme"}
				}
				if name == "outside" {
					aliases = []string{"reserved"}
				}
				_, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: model.RequestID("save_" + name)}, ID: model.ConfigurationProfileID(name), RevisionID: "one", Name: name, Desired: desired, Aliases: &aliases})
				require.NoError(t, err)
			}
			bundle := app.ConfigurationBundle{Format: app.ConfigurationBundleFormat, Version: 1, Profiles: []app.ConfigurationBundleEntry{{Key: "a", Name: "a", Desired: desired}, {Key: "b", Name: "b", Desired: desired, Aliases: []string{"moveme", "reserved"}}}}
			selections := []app.ConfigurationImportSelection{{Key: "a", ID: "a", RevisionID: "two", ExpectedRevision: 1, Name: "a"}, {Key: "b", ID: "b", RevisionID: "two", ExpectedRevision: 1, Name: "b"}}
			if gainFirst {
				selections[0], selections[1] = selections[1], selections[0]
			}
			req := app.ImportConfigurationsRequest{Context: app.RequestContext{Principal: op, RequestID: "move"}, Bundle: bundle, Selections: selections}
			_, err = service.ImportConfigurations(ctx, req)
			require.ErrorIs(t, err, app.ErrConflict)
			retained, err := service.ResolveConfigurationProfile(ctx, op, "moveme")
			require.NoError(t, err)
			require.Equal(t, model.ConfigurationProfileID("a"), retained.Profile.ID)
			require.Equal(t, model.Revision(1), retained.Profile.Revision)
			req.Bundle.Profiles[1].Aliases = []string{"moveme"}
			imported, err := service.ImportConfigurations(ctx, req)
			require.NoError(t, err)
			moved, err := service.ResolveConfigurationProfile(ctx, op, "moveme")
			require.NoError(t, err)
			require.Equal(t, model.ConfigurationProfileID("b"), moved.Profile.ID)
			replay, err := service.ImportConfigurations(ctx, req)
			require.NoError(t, err)
			require.True(t, replay.Repeated)
			require.Equal(t, imported.Profiles, replay.Profiles)
		})
	}
}
