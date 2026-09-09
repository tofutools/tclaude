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
