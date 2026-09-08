//go:build linux || darwin

package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestTeamSavedProfileFollowsEditsForNewDeploymentAndRetainsRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	service := app.New(store, providers.NewRegistry(&preparedWorkProvider{}))
	op := model.OperatorPrincipal()
	profileRequest := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile_one"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Startup: &model.ProfileStartup{Context: "Original profile context", InitialMessage: "Original instruction"}}
	profile, err := service.SaveConfigurationProfile(ctx, profileRequest)
	require.NoError(t, err)
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", ProfileID: "worker"}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
	draft := app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Team", Source: "profile fixture", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: op, RequestID: "save_team"}, Draft: draft})
	require.NoError(t, err)
	team.Members[0].Desired.Model = "ambiguous"
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: op, Draft: draft})
	require.ErrorIs(t, err, app.ErrInvalid)
	var firstRequest app.DeployTeamRequest
	var first app.TeamDeploymentResult
	for _, version := range []string{"first", "second"} {
		if version == "second" {
			profileRequest.Context.RequestID = "profile_two"
			profileRequest.RevisionID = "two"
			profileRequest.ExpectedRevision = profile.Profile.Revision
			profileRequest.Desired.Model = "second"
			profileRequest.Startup = &model.ProfileStartup{Context: "Updated profile context"}
			_, err = service.SaveConfigurationProfile(ctx, profileRequest)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			service = app.New(store, providers.NewRegistry(&preparedWorkProvider{}))
		}
		cwd := t.TempDir()
		now := time.Now().UTC()
		require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: model.WorkspaceID(version), State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
		req := app.DeployTeamRequest{Context: app.RequestContext{Principal: op, RequestID: model.RequestID(version)}, DeploymentID: model.DeploymentID(version), Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: model.GroupID(version), Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: model.WorkspaceID(version), ExpectedRevision: 1}}}}
		deployed, err := service.DeployTeam(ctx, req)
		require.NoError(t, err)
		agent, err := store.Agent(ctx, deployed.Deployment.Members["worker"])
		require.NoError(t, err)
		require.Equal(t, version, agent.Desired.Model)
		require.Equal(t, cwd, agent.Desired.WorkingDirectory)
		require.Equal(t, *profileRequest.Startup, deployed.Deployment.MemberStartups["worker"])
		work, err := store.WorkRun(ctx, deployed.Deployment.WorkRunID)
		require.NoError(t, err)
		require.NotNil(t, work.Run.Graph)
		for _, node := range work.Run.Graph.Nodes {
			if node.Performer != nil && node.Performer.Agent != nil {
				require.Contains(t, node.Performer.Agent.Brief, profileRequest.Startup.Context)
				require.NotContains(t, node.Performer.Agent.Brief, "Original instruction")
			}
		}
		if version == "first" {
			firstRequest, first = req, deployed
		}
	}
	replay, err := service.DeployTeam(ctx, firstRequest)
	require.NoError(t, err)
	require.Equal(t, first.Deployment.MemberStartups, replay.Deployment.MemberStartups)
	agent, err := store.Agent(ctx, replay.Deployment.Members["worker"])
	require.NoError(t, err)
	require.Equal(t, "first", agent.Desired.Model)
	require.Equal(t, model.ConfigurationProfileRevisionID("one"), agent.ConfigurationProfile.RevisionID)
}
