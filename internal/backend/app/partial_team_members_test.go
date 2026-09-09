//go:build linux || darwin

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
)

func TestPartialTeamMembersResolveCurrentDefaultsAtDeployment(t *testing.T) {
	ctx := context.Background()
	store, service, _ := regressionService(t)
	op := model.OperatorPrincipal()
	explicitModel := "explicit-model"
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{
		{Key: "inherited", Name: "Inherited", Options: &model.ConfigurationOptions{}},
		{Key: "explicit", Name: "Explicit", Options: &model.ConfigurationOptions{Model: &explicitModel}},
	}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"inherited", "explicit"}}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: effect(op, "save"), Draft: app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Source: "partial members", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}})
	require.NoError(t, err, "saving partial members needs neither configured providers nor ambient defaults")
	service = app.New(store, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}}))
	harness, modelName := "prepared-work", "first"
	profile := app.SaveConfigurationProfileRequest{Context: effect(op, "defaults_one"), ID: "defaults", RevisionID: "one", Name: "Defaults", Options: &model.ConfigurationOptions{Harness: &harness, Model: &modelName}}
	firstProfile, err := service.SaveConfigurationProfile(ctx, profile)
	require.NoError(t, err)
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "select"), Global: &firstProfile.Revision.Ref})
	require.NoError(t, err)
	now, cwd := time.Now().UTC(), t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	var first app.DeployTeamRequest
	var firstResult app.TeamDeploymentResult
	for _, version := range []string{"first", "second"} {
		if version == "second" {
			modelName = "second"
			profile.Context.RequestID = "defaults_two"
			profile.RevisionID = "two"
			profile.ExpectedRevision = 1
			_, err = service.SaveConfigurationProfile(ctx, profile)
			require.NoError(t, err)
		}
		req := app.DeployTeamRequest{Context: effect(op, model.RequestID("deploy_"+version)), DeploymentID: model.DeploymentID("deployment_" + version), Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: model.GroupID("group_" + version), Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}}
		deployed, deployErr := service.DeployTeam(ctx, req)
		require.NoError(t, deployErr)
		for _, key := range []string{"inherited", "explicit"} {
			agent, readErr := store.Agent(ctx, deployed.Deployment.Members[key])
			require.NoError(t, readErr)
			require.Equal(t, harness, agent.Desired.Harness)
			require.Equal(t, cwd, agent.Desired.WorkingDirectory)
			require.Nil(t, agent.ConfigurationProfile)
			if key == "explicit" {
				require.Equal(t, explicitModel, agent.Desired.Model)
			} else {
				require.Equal(t, version, agent.Desired.Model)
			}
		}
		if version == "first" {
			first, firstResult = req, deployed
		}
	}
	replay, err := service.DeployTeam(ctx, first)
	require.NoError(t, err)
	require.Equal(t, firstResult.Deployment.Members, replay.Deployment.Members)
	agent, err := store.Agent(ctx, replay.Deployment.Members["inherited"])
	require.NoError(t, err)
	require.Equal(t, "first", agent.Desired.Model)

	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "existing", Name: "Existing"})
	require.NoError(t, err)
	groupModel := "group-model"
	groupProfile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "group_profile"), ID: "group_profile", RevisionID: "one", Name: "Group defaults", Options: &model.ConfigurationOptions{Model: &groupModel}})
	require.NoError(t, err)
	_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "existing", Profile: &groupProfile.Revision.Ref})
	require.NoError(t, err)
	reinforcement := first
	reinforcement.Context.RequestID = "reinforce"
	reinforcement.DeploymentID = "reinforcement"
	reinforcement.Instantiation.GroupID = ""
	reinforcement.Instantiation.Target = model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "existing"}
	reinforced, err := service.DeployTeam(ctx, reinforcement)
	require.NoError(t, err)
	inherited, err := store.Agent(ctx, reinforced.Deployment.Members["inherited"])
	require.NoError(t, err)
	require.Equal(t, groupModel, inherited.Desired.Model, "group default wins over current global default")
	require.Equal(t, harness, inherited.Desired.Harness, "omitted group harness still inherits global default")
	explicit, err := store.Agent(ctx, reinforced.Deployment.Members["explicit"])
	require.NoError(t, err)
	require.Equal(t, explicitModel, explicit.Desired.Model, "explicit member value wins over both defaults")
	original, err := store.DefinitionRevision(ctx, saved.Revision.ID)
	require.NoError(t, err)
	require.Equal(t, team, *original.Team)
}

func TestPartialTeamMembersRejectMixedLaunchSources(t *testing.T) {
	_, service, _ := regressionService(t)
	for _, member := range []model.TeamMemberSpec{
		{Key: "worker", Name: "Worker", Options: &model.ConfigurationOptions{}, ProfileID: "profile"},
		{Key: "worker", Name: "Worker", Options: &model.ConfigurationOptions{}, Overrides: &model.TeamProfileOverrides{}},
		{Key: "worker", Name: "Worker", Options: &model.ConfigurationOptions{}, Desired: model.DesiredConfiguration{Harness: "claude"}},
	} {
		team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{member}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
		_, err := service.SaveDefinition(context.Background(), app.SaveDefinitionRequest{Context: effect(model.OperatorPrincipal(), "save"), Draft: app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Source: "mixed settings", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}

func TestPartialTeamMembersFenceDefaultsAtAdmission(t *testing.T) {
	for _, change := range []string{"global_profile", "global_selection", "group_profile", "group_selection"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			store, _, _ := regressionService(t)
			wrapped := &operatorOnlyTeamRaceStore{Store: store}
			service := app.New(wrapped, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}}))
			op := model.OperatorPrincipal()
			harness, beforeModel, afterModel := "prepared-work", "before", "after"
			profileRequest := app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "profile", RevisionID: "one", Name: "Profile", Options: &model.ConfigurationOptions{Harness: &harness, Model: &beforeModel}}
			profile, err := service.SaveConfigurationProfile(ctx, profileRequest)
			require.NoError(t, err)
			replacement, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "replacement"), ID: "replacement", RevisionID: "one", Name: "Replacement", Options: &model.ConfigurationOptions{Harness: &harness, Model: &afterModel}})
			require.NoError(t, err)
			_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "defaults"), Global: &profile.Revision.Ref})
			require.NoError(t, err)
			isGroup := change == "group_profile" || change == "group_selection"
			if isGroup {
				_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "existing", Name: "Existing"})
				require.NoError(t, err)
				_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "existing", Profile: &profile.Revision.Ref})
				require.NoError(t, err)
			}
			team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Options: &model.ConfigurationOptions{}}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: effect(op, "team"), Draft: app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Source: "partial", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}})
			require.NoError(t, err)
			now := time.Now().UTC()
			require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
			req := app.DeployTeamRequest{Context: effect(op, "deploy"), DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "created", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}}
			if isGroup {
				req.Instantiation.GroupID = ""
				req.Instantiation.Target = model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "existing"}
			}
			wrapped.before = func() {
				switch change {
				case "global_profile", "group_profile":
					profileRequest.Context.RequestID = "edit"
					profileRequest.ExpectedRevision = 1
					profileRequest.RevisionID = "two"
					profileRequest.Options.Model = &afterModel
					_, err = service.SaveConfigurationProfile(ctx, profileRequest)
				case "global_selection":
					_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "switch"), ExpectedRevision: 1, Global: &replacement.Revision.Ref})
				case "group_selection":
					_, err = service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "existing", ExpectedRevision: 1, Profile: &replacement.Revision.Ref})
				}
				require.NoError(t, err)
			}
			_, err = service.DeployTeam(ctx, req)
			require.ErrorIs(t, err, app.ErrConflict)
			snapshot, err := store.Snapshot(ctx)
			require.NoError(t, err)
			require.Empty(t, snapshot.Agents, "stale resolution must not publish agents")
			_, err = store.TeamDeployment(ctx, req.DeploymentID)
			require.ErrorIs(t, err, app.ErrNotFound)
			deployed, err := service.DeployTeam(ctx, req)
			require.NoError(t, err, "same request can retry with current defaults")
			agent, err := store.Agent(ctx, deployed.Deployment.Members["worker"])
			require.NoError(t, err)
			require.Equal(t, afterModel, agent.Desired.Model)
		})
	}
}
