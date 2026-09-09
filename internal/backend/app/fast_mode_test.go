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

type fastModeProvider struct{ *fakeProvider }

func (*fastModeProvider) Name() string { return "codex" }
func (p *fastModeProvider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.fakeProvider.Prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	attempt := prepared.(*fakePrepared)
	attempt.description.Evidence.Provider = "codex"
	p.runtime.evidence = attempt.description.Evidence
	return attempt, nil
}

func TestFastModePersistsOnAgentAndAdmittedExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := &fastModeProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(provider))
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "first", FastMode: model.FastModeOn, WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
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
	require.Equal(t, model.FastModeOn, provider.lastPreparation.Spec.FastMode)
	req.Context.RequestID = "profile_second"
	req.RevisionID = "second"
	req.ExpectedRevision = 1
	req.Desired.Model = "second"
	req.Desired.FastMode = model.FastModeOff
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
	require.Equal(t, model.FastModeOff, durableAgent.Desired.FastMode)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, "first", execution.Spec.Model)
	require.Equal(t, model.FastModeOn, execution.Spec.FastMode)
	require.Equal(t, &profile.Revision.Ref, execution.Spec.ConfigurationProfile)
}
func TestTeamFastModeRejectsInvalidBeforeDefinitionSave(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "claude", RevisionID: "one", Name: "Codex", Desired: model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	deny, unknown := model.FastModeOn, model.FastMode("unknown")
	foreign := "claude"
	members := []model.TeamMemberSpec{
		{Desired: model.DesiredConfiguration{Harness: "claude", FastMode: deny}},
		{Desired: model.DesiredConfiguration{Harness: "codex", FastMode: unknown}},
		{ProfileID: "claude", Overrides: &model.TeamProfileOverrides{FastMode: &deny}},
		{ProfileID: "claude", Overrides: &model.TeamProfileOverrides{FastMode: &unknown}},
		{ProfileID: "claude", Overrides: &model.TeamProfileOverrides{Harness: &foreign, FastMode: &deny}},
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
