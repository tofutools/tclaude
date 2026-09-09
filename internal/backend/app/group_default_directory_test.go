package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestGroupDefaultDirectoryPersistsOverridesAndExactRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry()).WithDirectoryDefaults(host.DirectoryBrowser{})
	op := model.OperatorPrincipal()
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	profile, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "profile", RevisionID: "one", Name: "Profile", Desired: desired})
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "future")
	defaults, err := svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", Profile: &profile.Revision.Ref, DefaultDirectory: &dir})
	require.NoError(t, err)
	require.Equal(t, dir, defaults.DefaultDirectory)
	// Older callers updating only the profile/environment preserve directory intent.
	defaults, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", Profile: &profile.Revision.Ref, ExpectedRevision: defaults.Revision})
	require.NoError(t, err)
	require.Equal(t, dir, defaults.DefaultDirectory)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry()).WithDirectoryDefaults(host.DirectoryBrowser{})
	got, err := svc.GetGroupConfiguration(ctx, op, "group")
	require.NoError(t, err)
	require.Equal(t, defaults, got)
	in := app.CreateGroupMemberRequest{Context: effect(op, "member"), ID: "member", Name: "Member", GroupID: "group", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision}
	member, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.Equal(t, dir, member.Agent.Desired.WorkingDirectory)
	explicit := t.TempDir()
	next := in
	next.Context.RequestID = "explicit"
	next.ID = "explicit"
	next.ExpectedGroupRevision = member.Group.Revision
	next.ConfigurationOverrides = &model.ConfigurationOptions{WorkingDirectory: &explicit}
	other, err := svc.CreateGroupMember(ctx, next)
	require.NoError(t, err)
	require.Equal(t, explicit, other.Agent.Desired.WorkingDirectory)
	clear := ""
	defaults, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", Profile: &profile.Revision.Ref, DefaultDirectory: &clear, ExpectedRevision: defaults.Revision})
	require.NoError(t, err)
	require.Empty(t, defaults.DefaultDirectory)
	replay, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, dir, replay.Agent.Desired.WorkingDirectory)
	next.Context.RequestID = "cleared"
	next.ID = "cleared"
	next.ExpectedGroupRevision = other.Group.Revision
	next.ExpectedDefaultRevision = defaults.Revision
	next.ConfigurationOverrides = &model.ConfigurationOptions{WorkingDirectory: &clear}
	created, err := svc.CreateGroupMember(ctx, next)
	require.NoError(t, err)
	require.Equal(t, desired.WorkingDirectory, created.Agent.Desired.WorkingDirectory)
	saved, err := store.ConfigurationProfile(ctx, "profile", "")
	require.NoError(t, err)
	require.Equal(t, desired, saved.Revision.Desired)
}

func TestGroupDefaultDirectoryClearFallsBackToDaemonDirectory(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	op := model.OperatorPrincipal()
	daemonDir := t.TempDir()
	svc := app.New(store, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}})).WithDirectoryDefaults(fixedDirectoryDefaults{daemonDir})
	_, err := svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	groupDir := t.TempDir()
	defaults, err := svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", DefaultDirectory: &groupDir})
	require.NoError(t, err)
	harness := "prepared-work"
	in := app.CreateGroupMemberRequest{Context: effect(op, "first"), ID: "first", Name: "First", GroupID: "group", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision, ConfigurationOverrides: &model.ConfigurationOptions{Harness: &harness}}
	first, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.Equal(t, groupDir, first.Agent.Desired.WorkingDirectory)
	clear := ""
	defaults, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", DefaultDirectory: &clear, ExpectedRevision: defaults.Revision})
	require.NoError(t, err)
	in.Context.RequestID = "second"
	in.ID = "second"
	in.ExpectedGroupRevision = first.Group.Revision
	in.ExpectedDefaultRevision = defaults.Revision
	in.ConfigurationOverrides.WorkingDirectory = &clear
	second, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.Equal(t, daemonDir, second.Agent.Desired.WorkingDirectory)
}

type fixedDirectoryDefaults struct{ cwd string }

func (f fixedDirectoryDefaults) NormalizeDefaultDirectory(ctx context.Context, path string) (string, error) {
	return host.DirectoryBrowser{}.NormalizeDefaultDirectory(ctx, path)
}
func (f fixedDirectoryDefaults) DefaultWorkingDirectory(context.Context) (string, error) {
	return f.cwd, nil
}
