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
