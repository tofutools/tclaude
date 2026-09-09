//go:build linux || darwin

package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
)

func TestGroupMemberSelectedProfileLayersAndCurrentIdentity(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	svc := app.New(store, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}}))
	op := model.OperatorPrincipal()
	_, err := svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	harness, cwd, globalModel, groupModel, selectedModel := "prepared-work", t.TempDir(), "global", "group", "selected"
	global, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "global"), ID: "global", RevisionID: "one", Name: "Global", Options: &model.ConfigurationOptions{Harness: &harness, WorkingDirectory: &cwd, Model: &globalModel, Environment: model.Environment{"GLOBAL": "yes", "VALUE": "global"}}, Startup: &model.ProfileStartup{Description: "Global description"}})
	require.NoError(t, err)
	_, err = svc.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "defaults"), Global: &global.Revision.Ref})
	require.NoError(t, err)
	group, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "group"), ID: "group", RevisionID: "one", Name: "Group", Options: &model.ConfigurationOptions{Model: &groupModel, Environment: model.Environment{"GROUP": "yes", "VALUE": "group"}}, Startup: &model.ProfileStartup{Role: "reviewer"}})
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &group.Revision.Ref, Environment: model.Environment{"MEMBERSHIP": "yes", "VALUE": "membership"}})
	require.NoError(t, err)
	save := app.SaveConfigurationProfileRequest{Context: effect(op, "selected"), ID: "selected", RevisionID: "one", Name: "Selected", Options: &model.ConfigurationOptions{Model: &selectedModel, Environment: model.Environment{"SELECTED": "yes", "VALUE": "selected"}}}
	selected, err := svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	req := app.CreateGroupMemberRequest{Context: effect(op, "member"), ID: "child", Name: "Child", GroupID: "team", ProfileID: "selected", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1, Environment: model.Environment{"VALUE": "explicit"}}
	first, err := svc.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "selected", first.Agent.Desired.Model)
	require.Equal(t, cwd, first.Agent.Desired.WorkingDirectory)
	require.Equal(t, model.Environment{"GLOBAL": "yes", "GROUP": "yes", "SELECTED": "yes", "MEMBERSHIP": "yes", "VALUE": "explicit"}, first.Agent.Desired.Environment)
	require.Equal(t, selected.Revision.Ref, *first.Agent.ConfigurationProfile)
	require.Equal(t, model.AgentDisplayLabels{Role: "reviewer", Description: "Global description"}, first.Agent.Labels.InGroup("team"))
	selectedModel = "updated"
	save.Context.RequestID = "edit"
	save.RevisionID = "two"
	save.ExpectedRevision = 1
	_, err = svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	replay, err := svc.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, first.Agent, replay.Agent)
	changed := req
	changed.ProfileID = "global"
	_, err = svc.CreateGroupMember(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	next := req
	next.ID = "next"
	next.Context.RequestID = "next"
	next.ExpectedGroupRevision = first.Group.Revision
	explicit := "operator"
	next.ConfigurationOverrides = &model.ConfigurationOptions{Model: &explicit}
	next.Labels = &model.AgentDisplayLabels{}
	second, err := svc.CreateGroupMember(ctx, next)
	require.NoError(t, err)
	require.Equal(t, "operator", second.Agent.Desired.Model)
	require.Equal(t, model.ConfigurationProfileRevisionID("two"), second.Agent.ConfigurationProfile.RevisionID)
	require.Empty(t, second.Agent.Labels.InGroup("team"))
}

func TestGroupMemberSelectedProfileFencesEveryMutableTier(t *testing.T) {
	for _, tier := range []string{"selected", "group", "global"} {
		t.Run(tier, func(t *testing.T) {
			ctx := context.Background()
			store, _, _ := regressionService(t)
			provider := &partialTeamProvider{&preparedWorkProvider{}}
			svc := app.New(store, providers.NewRegistry(provider))
			op := model.OperatorPrincipal()
			_, err := svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
			require.NoError(t, err)
			harness, cwd := "prepared-work", t.TempDir()
			saves := map[string]app.SaveConfigurationProfileRequest{}
			refs := map[string]model.ConfigurationProfileRef{}
			for _, id := range []string{"global", "group", "selected"} {
				save := app.SaveConfigurationProfileRequest{Context: effect(op, model.RequestID(id)), ID: model.ConfigurationProfileID(id), RevisionID: "one", Name: id, Options: &model.ConfigurationOptions{}}
				if id == "global" {
					save.Options = &model.ConfigurationOptions{Harness: &harness, WorkingDirectory: &cwd}
				}
				result, err := svc.SaveConfigurationProfile(ctx, save)
				require.NoError(t, err)
				saves[id] = save
				refs[id] = result.Revision.Ref
			}
			global, group := refs["global"], refs["group"]
			_, err = svc.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "defaults"), Global: &global})
			require.NoError(t, err)
			_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &group})
			require.NoError(t, err)
			wrapped := &editingGroupProfileStore{Store: store, before: func() {
				save := saves[tier]
				save.Context.RequestID = "edit"
				save.RevisionID = "two"
				save.ExpectedRevision = 1
				save.Name = "Updated"
				_, err := svc.SaveConfigurationProfile(ctx, save)
				require.NoError(t, err)
			}}
			req := app.CreateGroupMemberRequest{Context: effect(op, "member"), GroupID: "team", ID: "child", Name: "Child", ProfileID: "selected", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1}
			_, err = app.New(wrapped, providers.NewRegistry(provider)).CreateGroupMember(ctx, req)
			require.ErrorIs(t, err, app.ErrConflict)
			_, err = store.Agent(ctx, "child")
			require.ErrorIs(t, err, app.ErrNotFound)
			_, err = svc.CreateGroupMember(ctx, req)
			require.NoError(t, err)
		})
	}
}
