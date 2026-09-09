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
	"github.com/tofutools/tclaude/internal/backend/ports"
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
	profileRequest := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile_one"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "first", Effort: "low", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Startup: &model.ProfileStartup{Context: "Original profile context", InitialMessage: "Original instruction"}}
	profile, err := service.SaveConfigurationProfile(ctx, profileRequest)
	require.NoError(t, err)
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", ProfileID: "worker"}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
	overrideModel, clearEffort := "custom", ""
	team.Members = append(team.Members, model.TeamMemberSpec{Key: "custom", Name: "Custom", ProfileID: "worker", Overrides: &model.TeamProfileOverrides{Model: &overrideModel, Effort: &clearEffort}})
	team.Waves[0].MemberKeys = append(team.Waves[0].MemberKeys, "custom")
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
			profileRequest.Desired.Effort = "high"
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
		require.Equal(t, profileRequest.Desired.Effort, agent.Desired.Effort)
		custom, err := store.Agent(ctx, deployed.Deployment.Members["custom"])
		require.NoError(t, err)
		require.Equal(t, "custom", custom.Desired.Model)
		require.Empty(t, custom.Desired.Effort)
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

type partialTeamProvider struct{ *preparedWorkProvider }

func (p *partialTeamProvider) Capabilities() ports.ProviderCapabilities {
	capabilities := p.preparedWorkProvider.Capabilities()
	capabilities.LaunchPolicy = &ports.PolicyRequirements{DefaultApproval: model.ApprovalAutomatic, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalAutomatic}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}
	return capabilities
}

func TestTeamPartialProfileUsesExplicitMemberHarnessAndWorkspace(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}}))
	op := model.OperatorPrincipal()
	profileModel := "portable-model"
	profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "portable", RevisionID: "one", Name: "Portable", Options: &model.ConfigurationOptions{Model: &profileModel}, Startup: &model.ProfileStartup{Context: "Reusable guidance"}})
	require.NoError(t, err)
	harness := "prepared-work"
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", ProfileID: "portable", Overrides: &model.TeamProfileOverrides{Harness: &harness}}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: effect(op, "team"), Draft: app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Source: "partial profile fixture", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}})
	require.NoError(t, err)
	cwd, now := t.TempDir(), time.Now().UTC()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	req := app.DeployTeamRequest{Context: effect(op, "deploy"), DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}}
	deployed, err := service.DeployTeam(ctx, req)
	require.NoError(t, err, "the explicit member harness must precede fallback lookup; Claude is not configured")
	agent, err := store.Agent(ctx, deployed.Deployment.Members["worker"])
	require.NoError(t, err)
	require.Equal(t, harness, agent.Desired.Harness)
	require.Equal(t, profileModel, agent.Desired.Model)
	require.Equal(t, cwd, agent.Desired.WorkingDirectory)
	require.Equal(t, "Reusable guidance", deployed.Deployment.MemberStartups["worker"].Context)
	reopened, err := service.GetConfigurationProfile(ctx, op, profile.Revision.Ref)
	require.NoError(t, err)
	require.Equal(t, profile, reopened)
	replay, err := service.DeployTeam(ctx, req)
	require.NoError(t, err)
	require.Equal(t, deployed.Deployment.ID, replay.Deployment.ID)
	require.Equal(t, deployed.Deployment.Members, replay.Deployment.Members)
	require.Equal(t, deployed.Deployment.Workspaces, replay.Deployment.Workspaces)
	require.Equal(t, deployed.Deployment.MemberStartups, replay.Deployment.MemberStartups)
}
