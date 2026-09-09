package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

type openCodeToolsProvider struct{ *fakeProvider }

func (*openCodeToolsProvider) Name() string { return "opencode" }
func (p *openCodeToolsProvider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.fakeProvider.Prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	attempt := prepared.(*fakePrepared)
	attempt.description.Evidence.Provider = "opencode"
	p.runtime.evidence = attempt.description.Evidence
	return attempt, nil
}

func TestToolGovernancePersistsOnAgentAndAdmittedExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := &openCodeToolsProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(provider))
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "opencode", Model: "first", ToolGovernance: model.ToolGovernanceDeny, WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	req := app.SaveConfigurationProfileRequest{Context: effect(operator, "profile_first"), ID: "profile", RevisionID: "first", Name: "Worker", Desired: desired}
	profile, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	mixed := app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: desired, ConfigurationProfile: &profile.Revision.Ref}
	_, err = service.CreateAgent(ctx, mixed)
	require.ErrorIs(t, err, app.ErrInvalid)
	mixed.Desired = model.DesiredConfiguration{}
	worker, err := service.CreateAgent(ctx, mixed)
	require.NoError(t, err)
	require.Equal(t, desired, worker.Agent.Desired)
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_selected"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: worker.Agent.ID, ExpectedRevision: worker.Agent.Revision}}})
	require.NoError(t, err)
	require.Equal(t, &profile.Revision.Ref, launched.Execution.Spec.ConfigurationProfile)
	require.Equal(t, model.ToolGovernanceDeny, provider.lastPreparation.Spec.ToolGovernance)
	req.Context.RequestID = "profile_second"
	req.RevisionID = "second"
	req.ExpectedRevision = 1
	req.Desired.Model = "second"
	req.Desired.ToolGovernance = model.ToolGovernanceAsk
	newer, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	current, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	updated, err := service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: operator, ID: current.ID, ExpectedRevision: current.Revision, Name: current.Name, ConfigurationProfile: &newer.Revision.Ref})
	require.NoError(t, err)
	require.Equal(t, "second", updated.Agent.Desired.Model)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	durableAgent, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, &newer.Revision.Ref, durableAgent.ConfigurationProfile)
	require.Equal(t, model.ToolGovernanceAsk, durableAgent.Desired.ToolGovernance)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, "first", execution.Spec.Model)
	require.Equal(t, model.ToolGovernanceDeny, execution.Spec.ToolGovernance)
	require.Equal(t, &profile.Revision.Ref, execution.Spec.ConfigurationProfile)
}

func TestToolGovernanceRejectsUnknownAndForeignConfigurationBeforeSave(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	for _, tc := range []struct {
		harness string
		tools   model.ToolGovernance
	}{{"opencode", "unknown"}, {"claude", model.ToolGovernanceDeny}, {"codex", model.ToolGovernanceAsk}, {"copilot", model.ToolGovernanceAllow}} {
		_, err = service.CreateAgent(context.Background(), app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "invalid", Name: "Invalid", Desired: model.DesiredConfiguration{Harness: tc.harness, Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined, ToolGovernance: tc.tools}})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	snapshot, err := service.Snapshot(context.Background(), app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, snapshot.Agents)
}

func TestTeamToolGovernanceRejectsInvalidBeforeDefinitionSave(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "codex", RevisionID: "one", Name: "Codex", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	deny, unknown := model.ToolGovernanceDeny, model.ToolGovernance("unknown")
	foreign := "claude"
	members := []model.TeamMemberSpec{
		{Desired: model.DesiredConfiguration{Harness: "codex", ToolGovernance: deny}},
		{Desired: model.DesiredConfiguration{Harness: "opencode", ToolGovernance: unknown}},
		{ProfileID: "codex", Overrides: &model.TeamProfileOverrides{ToolGovernance: &deny}},
		{ProfileID: "codex", Overrides: &model.TeamProfileOverrides{ToolGovernance: &unknown}},
		{ProfileID: "codex", Overrides: &model.TeamProfileOverrides{Harness: &foreign, ToolGovernance: &deny}},
	}
	for _, member := range members {
		member.Key = "worker"
		member.Name = "Worker"
		draft := app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Source: "fixture", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{member}, Waves: []model.TeamWave{{ID: "first", MemberKeys: []string{"worker"}}}}}
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: op, Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
		_, err = service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: effect(op, "save_team"), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	_, err = store.Definition(ctx, "team")
	require.ErrorIs(t, err, app.ErrNotFound)
}
