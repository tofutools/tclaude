//go:build linux || darwin

package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"testing"
)

func TestGroupMemberWithoutGroupProfileUsesCurrentGlobalOrProviderDefaults(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider", true: "global"}[global], func(t *testing.T) {
			ctx := context.Background()
			store, _, _ := regressionService(t)
			service := app.New(store, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}}))
			op := model.OperatorPrincipal()
			_, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
			require.NoError(t, err)
			harness, cwd := "prepared-work", t.TempDir()
			options := model.ConfigurationOptions{Harness: &harness, WorkingDirectory: &cwd}
			var profile app.ConfigurationProfileResult
			if global {
				name := "first"
				profile, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "global", RevisionID: "one", Name: "Global", Options: &model.ConfigurationOptions{Harness: &harness, Model: &name, WorkingDirectory: &cwd}, Startup: &model.ProfileStartup{Role: "reviewer"}})
				require.NoError(t, err)
				_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "default"), Global: &profile.Revision.Ref})
				require.NoError(t, err)
				options = model.ConfigurationOptions{}
			}
			req := app.CreateGroupMemberRequest{Context: effect(op, "member"), GroupID: "team", ID: "member", Name: "Member", ExpectedGroupRevision: 1, ConfigurationOverrides: &options}
			first, err := service.CreateGroupMember(ctx, req)
			require.NoError(t, err)
			require.Equal(t, harness, first.Agent.Desired.Harness)
			require.Equal(t, cwd, first.Agent.Desired.WorkingDirectory)
			require.Equal(t, model.ApprovalAutomatic, first.Agent.Desired.Approval)
			if global {
				require.Equal(t, "first", first.Agent.Desired.Model)
				require.Equal(t, "reviewer", first.Agent.Labels.InGroup("team").Role)
				name := "second"
				_, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "edit"), ID: "global", RevisionID: "two", ExpectedRevision: profile.Profile.Revision, Name: "Global", Options: &model.ConfigurationOptions{Harness: &harness, Model: &name, WorkingDirectory: &cwd}})
				require.NoError(t, err)
				next := req
				next.ID = "next"
				next.Context.RequestID = "next"
				next.ExpectedGroupRevision = first.Group.Revision
				second, err := service.CreateGroupMember(ctx, next)
				require.NoError(t, err)
				require.Equal(t, "second", second.Agent.Desired.Model)
			}
			replay, err := service.CreateGroupMember(ctx, req)
			require.NoError(t, err)
			require.True(t, replay.Repeated)
			require.Equal(t, first.Agent, replay.Agent)
		})
	}
}

func TestGroupMemberGlobalFallbackFencesConcurrentProfileEdit(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	provider := &partialTeamProvider{&preparedWorkProvider{}}
	service := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	_, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	harness, cwd, name := "prepared-work", t.TempDir(), "first"
	save := app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "global", RevisionID: "one", Name: "Global", Options: &model.ConfigurationOptions{Harness: &harness, Model: &name, WorkingDirectory: &cwd}}
	profile, err := service.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "default"), Global: &profile.Revision.Ref})
	require.NoError(t, err)
	wrapped := &editingGroupProfileStore{Store: store, before: func() {
		name = "second"
		save.Context.RequestID = "edit"
		save.RevisionID = "two"
		save.ExpectedRevision = profile.Profile.Revision
		_, err := service.SaveConfigurationProfile(ctx, save)
		require.NoError(t, err)
	}}
	request := app.CreateGroupMemberRequest{Context: effect(op, "member"), GroupID: "team", ID: "member", Name: "Member", ExpectedGroupRevision: 1}
	_, err = app.New(wrapped, providers.NewRegistry(provider)).CreateGroupMember(ctx, request)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "member")
	require.ErrorIs(t, err, app.ErrNotFound)
	member, err := service.CreateGroupMember(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "second", member.Agent.Desired.Model)
}
