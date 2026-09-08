package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestLaunchSandboxSelectionResolvesServerContentBeforeSavingAgent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry()).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "create"}, ID: "sandbox", Name: "Sandbox", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "literal"}}})
	require.NoError(t, err)
	scopes := []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: profile.Revision.Ref}}
	selected, err := service.ResolveLaunchSandbox(ctx, operator, scopes)
	require.NoError(t, err)
	require.NoError(t, selected.Validate())
	desired := model.DesiredConfiguration{HostSandbox: &selected, Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "agent", Name: "Agent", Desired: desired})
	require.NoError(t, err)
	require.Equal(t, &selected, created.Agent.Desired.HostSandbox)
	forged := selected.Clone()
	forged.PolicyHash = strings.Repeat("c", 64)
	desired.HostSandbox = &forged
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "forged", Name: "Forged", Desired: desired})
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "forged")
	require.ErrorIs(t, err, app.ErrNotFound)
	_, err = service.SetSandboxProfileArchived(ctx, app.SetSandboxProfileArchivedRequest{Context: app.RequestContext{Principal: operator, RequestID: "archive"}, ID: profile.Profile.ID, ExpectedRevision: profile.Profile.Revision, Archived: true})
	require.NoError(t, err)
	_, err = service.ResolveLaunchSandbox(ctx, operator, scopes)
	require.ErrorIs(t, err, app.ErrConflict)
	saved, err := store.Agent(ctx, "agent")
	require.NoError(t, err)
	require.Equal(t, &selected, saved.Desired.HostSandbox, "archiving a profile does not rewrite an existing pin")
}

func TestSandboxConfigurationSaveRetryDoesNotRequireFreshHostInspection(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry()).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "sandbox"}, ID: "sandbox", Name: "Sandbox", Policy: model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate}})
	require.NoError(t, err)
	selected, err := service.ResolveLaunchSandbox(ctx, operator, []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: profile.Revision.Ref}})
	require.NoError(t, err)
	req := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "configuration"}, ID: "configuration", RevisionID: "revision", Name: "Configuration", Desired: model.DesiredConfiguration{HostSandbox: &selected, Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}
	saved, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	// Restarted application has no host inspector. The committed receipt still
	// answers an unchanged retry, while fresh writes require normal preparation.
	service = app.New(store, providers.NewRegistry())
	repeated, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, saved, repeated)
	changed := req
	changed.Name = "Changed intent"
	_, err = service.SaveConfigurationProfile(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	fresh := req
	fresh.Context.RequestID = "fresh"
	_, err = service.SaveConfigurationProfile(ctx, fresh)
	require.ErrorIs(t, err, app.ErrUnavailable)
}
