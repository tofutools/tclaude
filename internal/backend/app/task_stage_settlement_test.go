package app_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// Review probe: after a consumed interaction fails durable completion, a later
// sweep in the same service must not leave the admitted operation pending forever.
func TestTaskStagesConsumedInputSettlesWithoutRestart(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	provider := &stageAgentProvider{runtimes: map[model.ExecutionID]*stageAgentRuntime{}}
	failing := &stageInteractionCompletionFailure{Store: store}
	service := app.New(failing, providers.NewRegistry(provider))
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: provider.Name(), Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.PlanApproval = nil
	graph.Nodes[0].Stages.Review = nil
	graph.Nodes[0].Stages.Checks = nil
	performer := model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", ContextPolicy: model.AgentContextReuse, Brief: "next work"}}
	graph.Nodes[0].Performer = &performer
	graph.Nodes[0].Stages.Plan.Performer = performer
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	run, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	plan := run.Run.NodeAttempts[0]
	_, err = service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "plan_done"}, Attempt: plan.Ref, ExpectedRunRevision: run.Run.Revision, Disposition: model.WorkOutcomeVerified, Detail: "plan ready"})
	require.NoError(t, err)
	failing.fail = true
	_, err = service.ReconcilePendingWork(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, provider.runtimes[plan.ExecutionID].inputs, 1)

	failing.fail = false
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptUncertain, run.Run.NodeAttempts[1].State)
	require.Len(t, provider.runtimes[plan.ExecutionID].inputs, 1)
}

func TestTaskStagesConcurrentSweepDoesNotAbandonInFlightInput(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	provider := &stageAgentProvider{runtimes: map[model.ExecutionID]*stageAgentRuntime{}}
	failing := &stageInteractionCompletionFailure{Store: store}
	service := app.New(failing, providers.NewRegistry(provider))
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: provider.Name(), Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.PlanApproval = nil
	graph.Nodes[0].Stages.Review = nil
	graph.Nodes[0].Stages.Checks = nil
	performer := model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", ContextPolicy: model.AgentContextReuse, Brief: "next work"}}
	graph.Nodes[0].Performer = &performer
	graph.Nodes[0].Stages.Plan.Performer = performer
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	run, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	plan := run.Run.NodeAttempts[0]
	_, err = service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "plan_done"}, Attempt: plan.Ref, ExpectedRunRevision: run.Run.Revision, Disposition: model.WorkOutcomeVerified, Detail: "plan ready"})
	require.NoError(t, err)

	runtime := provider.runtimes[plan.ExecutionID]
	runtime.inputEntered = make(chan struct{})
	release := make(chan struct{})
	runtime.inputRelease = release
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	result := make(chan error, 1)
	go func() { _, err := service.ReconcilePendingWork(ctx); result <- err }()
	select {
	case <-runtime.inputEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("native input was not entered")
	}
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptAdmitted, run.Run.NodeAttempts[1].State)
	releaseOnce.Do(func() { close(release) })
	select {
	case err = <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("native input did not finish")
	}
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptRunning, run.Run.NodeAttempts[1].State)
	require.Len(t, runtime.inputs, 1)
}
