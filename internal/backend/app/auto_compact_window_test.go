package app_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestAutoCompactWindowPersistsOnAgentAndAdmittedExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	provider := &autoCompactWindowProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(provider))
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "claude", Model: "first", AutoCompactWindow: "450000", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
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
	require.Equal(t, model.AutoCompactWindow("450000"), provider.lastPreparation.Spec.AutoCompactWindow)
	req.Context.RequestID = "profile_second"
	req.RevisionID = "second"
	req.ExpectedRevision = 1
	req.Desired.Model = "second"
	req.Desired.AutoCompactWindow = ""
	newer, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	current, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	updated, err := service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: operator, ID: current.ID, ExpectedRevision: current.Revision, Name: current.Name, ConfigurationProfile: &newer.Revision.Ref})
	require.NoError(t, err)
	require.Equal(t, "second", updated.Agent.Desired.Model)
	usage := model.ContextUsage{ModelWindow: 1000000, EffectiveWindow: 450000, NativePercent: 21, UsedPercent: 46.6666667, ObservedAt: time.Now().UTC()}
	raw, err := json.Marshal(usage)
	require.NoError(t, err)
	_, err = store.RecordRecovery(ctx, launched.Execution.ID, model.ExecutionRunning, nil, model.ProviderEvidence{Provider: "claude", Version: 1, Payload: raw}, time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	durableAgent, err := store.Agent(ctx, worker.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, &newer.Revision.Ref, durableAgent.ConfigurationProfile)
	require.Equal(t, model.AutoCompactWindow(""), durableAgent.Desired.AutoCompactWindow)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, "first", execution.Spec.Model)
	require.Equal(t, model.AutoCompactWindow("450000"), execution.Spec.AutoCompactWindow)
	require.Equal(t, &profile.Revision.Ref, execution.Spec.ConfigurationProfile)
	service = app.New(store, providers.NewRegistry(provider))
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Executions, 1)
	require.Equal(t, &usage, snapshot.Executions[0].ContextUsage)
}

type autoCompactWindowProvider struct{ *fakeProvider }

func (*autoCompactWindowProvider) Name() string { return "claude" }
func (p *autoCompactWindowProvider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.fakeProvider.Prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	attempt := prepared.(*fakePrepared)
	attempt.description.Evidence.Provider = "claude"
	p.runtime.evidence = attempt.description.Evidence
	return attempt, nil
}

func (*autoCompactWindowProvider) ProjectContextUsage(execution model.Execution) *model.ContextUsage {
	var result model.ContextUsage
	if json.Unmarshal(execution.Evidence.Payload, &result) != nil || result.ObservedAt.IsZero() {
		return nil
	}
	return &result
}
