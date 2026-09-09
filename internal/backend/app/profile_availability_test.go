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

func TestConfigurationAvailabilityPreservesDefaultsAndReason(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	request := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "save"}, ID: "worker", RevisionID: "first", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "claude", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}
	saved, err := service.SaveConfigurationProfile(ctx, request)
	require.NoError(t, err)
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: app.RequestContext{Principal: operator, RequestID: "defaults"}, Global: &saved.Revision.Ref})
	require.NoError(t, err)
	reason := "Provider maintenance"
	disable := app.SetConfigurationProfileAvailabilityRequest{Context: app.RequestContext{Principal: operator, RequestID: "disable"}, ID: "worker", ExpectedRevision: 1, Disabled: true, Reason: &reason}
	disabled, err := service.SetConfigurationProfileAvailability(ctx, disable)
	require.NoError(t, err)
	require.True(t, disabled.Disabled)
	retry, err := service.SetConfigurationProfileAvailability(ctx, disable)
	require.NoError(t, err)
	require.Equal(t, disabled, retry)
	defaults, err := service.GetConfigurationDefaults(ctx, operator)
	require.NoError(t, err)
	require.Equal(t, model.ConfigurationProfileID("worker"), defaults.Global.ProfileID)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "blocked", Name: "Blocked", ConfigurationDefault: "global"})
	var blocked *app.ProfileDisabledError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, reason, blocked.Reason)

	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", ProfileID: "worker"}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "team"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_one", Name: "Team", Source: "availability fixture", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}})
	require.NoError(t, err)
	_, err = service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "team_group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "missing", ExpectedRevision: 1}}}})
	require.ErrorAs(t, err, &blocked)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Groups)
	request.Context.RequestID = "edit"
	request.RevisionID = "second"
	request.ExpectedRevision = disabled.Revision
	request.Name = "Worker edited"
	edited, err := service.SaveConfigurationProfile(ctx, request)
	require.NoError(t, err)
	require.True(t, edited.Profile.Disabled)
	require.Equal(t, reason, edited.Profile.DisabledReason)
	enabled, err := service.SetConfigurationProfileAvailability(ctx, app.SetConfigurationProfileAvailabilityRequest{Context: app.RequestContext{Principal: operator, RequestID: "enable"}, ID: "worker", ExpectedRevision: edited.Profile.Revision})
	require.NoError(t, err)
	require.False(t, enabled.Disabled)
	require.Equal(t, reason, enabled.DisabledReason)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "allowed", Name: "Allowed", ConfigurationDefault: "global"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry())
	reopened, err := service.GetConfigurationProfile(ctx, operator, model.ConfigurationProfileRef{ProfileID: "worker"})
	require.NoError(t, err)
	require.Equal(t, enabled, reopened.Profile)
}
