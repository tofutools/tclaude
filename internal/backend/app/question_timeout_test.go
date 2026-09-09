package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestQuestionTimeoutPersistsLaunchAndExplicitNativeInherit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := &peerMessagingProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "claude", Model: "worker", WorkingDirectory: t.TempDir(), AskUserQuestionTimeout: "5m", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "profile", RevisionID: "one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "worker", Name: "Worker", ConfigurationProfile: &profile.Revision.Ref})
	require.NoError(t, err)
	request := app.LaunchRequest{RequestContext: effect(op, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}}}
	launched, err := service.Launch(ctx, request)
	require.NoError(t, err)
	require.Equal(t, model.AskUserQuestionTimeout("5m"), provider.lastPreparation.Spec.AskUserQuestionTimeout)
	current, err := store.Agent(ctx, agent.Agent.ID)
	require.NoError(t, err)
	desired.AskUserQuestionTimeout = "inherit"
	_, err = service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: current.ID, Name: current.Name, ExpectedRevision: current.Revision, Desired: desired})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	current, err = store.Agent(ctx, agent.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, model.AskUserQuestionTimeout("inherit"), current.Desired.AskUserQuestionTimeout)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, model.AskUserQuestionTimeout("5m"), execution.Spec.AskUserQuestionTimeout)
	replayed, err := app.New(store, providers.NewRegistry(provider)).Launch(ctx, request)
	require.NoError(t, err)
	require.Equal(t, launched.Execution.ID, replayed.Execution.ID)
	desired.Harness = "codex"
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "invalid", Name: "Invalid", Desired: desired})
	require.ErrorIs(t, err, app.ErrInvalid)
}
