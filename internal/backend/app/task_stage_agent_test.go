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

func TestTaskStagesAgentFreshAndReusedContext(t *testing.T) {
	for _, policy := range []model.AgentContextPolicy{model.AgentContextFresh, model.AgentContextReuse} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			provider := &stageAgentProvider{runtimes: map[model.ExecutionID]*stageAgentRuntime{}}
			service := app.New(store, providers.NewRegistry(provider))
			_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: provider.Name(), Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
			require.NoError(t, err)
			graph := stagedHumanGraph()
			graph.Nodes[0].Stages.PlanApproval = nil
			graph.Nodes[0].Stages.Review = nil
			graph.Nodes[0].Stages.Checks = graph.Nodes[0].Stages.Checks[:1]
			graph.Nodes[0].Performer = &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", ContextPolicy: policy, Brief: "Implement the task"}}
			graph.Nodes[0].Stages.Plan.Performer = model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", ContextPolicy: model.AgentContextFresh, Brief: "Prepare the plan"}}
			run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
			require.NoError(t, err)
			group := run.Run.Graph.TaskGroups[0]
			finish := func(node model.WorkNodeID, detail string) model.WorkNodeAttempt {
				t.Helper()
				for range 3 {
					_, err = service.ReconcilePendingWork(ctx)
					require.NoError(t, err)
				}
				run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
				require.NoError(t, err)
				var attempt model.WorkNodeAttempt
				for _, a := range run.Run.NodeAttempts {
					if a.Ref.NodeID == node && a.State == model.NodeAttemptRunning {
						attempt = a
					}
				}
				require.NotEmpty(t, attempt.Ref.IssuanceID)
				run, err = service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID("evidence_" + string(attempt.Ref.IssuanceID))}, Attempt: attempt.Ref, ExpectedRunRevision: run.Run.Revision, Disposition: model.WorkOutcomeVerified, Detail: detail})
				require.NoError(t, err)
				return attempt
			}
			plan := finish(group.Plan, "Use the validated transaction boundary")
			work := finish(group.Work, "Implementation is ready")
			check := attemptFor(t, run, group.Checks[0], work.Ref.Attempt)
			window := decisionByID(t, run, check.DecisionID)
			_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "reject"}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, Answer: "reject", Reason: "Fix the missing race check"})
			require.NoError(t, err)
			retry := finish(group.Work, "Race check corrected")
			if policy == model.AgentContextReuse {
				require.Equal(t, plan.ExecutionID, work.ExecutionID)
				require.Equal(t, work.ExecutionID, retry.ExecutionID)
				require.Len(t, provider.requests, 1)
				require.Len(t, provider.runtimes[plan.ExecutionID].inputs, 2)
				require.Contains(t, provider.runtimes[plan.ExecutionID].inputs[1], "Fix the missing race check")
			} else {
				require.NotEqual(t, plan.ExecutionID, work.ExecutionID)
				require.NotEqual(t, work.ExecutionID, retry.ExecutionID)
				require.Len(t, provider.requests, 3)
				require.True(t, provider.runtimes[plan.ExecutionID].stopped)
				require.True(t, provider.runtimes[work.ExecutionID].stopped)
				require.Contains(t, provider.requests[2].InitialInput.Body, "Fix the missing race check")
			}
			require.Contains(t, retry.Performer.Agent.Brief, "Use the validated transaction boundary")
			for range 3 {
				_, err = service.ReconcilePendingWork(ctx)
				require.NoError(t, err)
			}
			if policy == model.AgentContextReuse {
				require.Len(t, provider.runtimes[plan.ExecutionID].inputs, 2, "settled inputs never replay")
			}
		})
	}
}

type stageAgentProvider struct {
	requests []ports.PreparationRequest
	runtimes map[model.ExecutionID]*stageAgentRuntime
}

func (*stageAgentProvider) Name() string { return "prepared-work" }
func (*stageAgentProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{PreparedInitialInput: true}
}
func (p *stageAgentProvider) Prepare(_ context.Context, in ports.PreparationRequest) (ports.PreparedAttempt, error) {
	p.requests = append(p.requests, in)
	return &stageAgentPreparation{preparedWorkAttempt: preparedWorkAttempt{request: in}, provider: p}, nil
}
func (p *stageAgentProvider) Recover(_ context.Context, in ports.RecoveryRequest) (ports.RecoveryResult, error) {
	runtime := p.runtimes[in.ExecutionID]
	if runtime == nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
	}
	observation, _ := runtime.Observe(context.Background())
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: runtime, Observation: observation, Evidence: observation.Evidence}, nil
}

type stageAgentPreparation struct {
	preparedWorkAttempt
	provider *stageAgentProvider
}

func (p *stageAgentPreparation) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	result, err := p.preparedWorkAttempt.Release(ctx, permit)
	if err != nil {
		return result, err
	}
	runtime := &stageAgentRuntime{Runtime: result.Runtime}
	p.provider.runtimes[runtime.ExecutionID()] = runtime
	result.Runtime = runtime
	return result, nil
}

type stageAgentRuntime struct {
	ports.Runtime
	inputs       []string
	stopped      bool
	inputEntered chan struct{}
	inputRelease <-chan struct{}
}

func (r *stageAgentRuntime) Interact(_ context.Context, in ports.Interaction) (ports.InteractionResult, error) {
	if r.inputEntered != nil {
		close(r.inputEntered)
		<-r.inputRelease
	}
	r.inputs = append(r.inputs, in.Text)
	return ports.InteractionResult{Disposition: ports.EffectAccepted}, nil
}
func (r *stageAgentRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	r.stopped = true
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true}, nil
}
func (r *stageAgentRuntime) Observe(context.Context) (ports.Observation, error) {
	state := ports.WorkloadRunning
	if r.stopped {
		state = ports.WorkloadExited
	}
	return ports.Observation{Workload: state, Context: ports.ContextReady, ObservedAt: time.Now()}, nil
}

func TestTaskStagesConsumedNativeInputIsNotReplayedAfterRestart(t *testing.T) {
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
	restarted := app.New(store, providers.NewRegistry(provider))
	_, err = restarted.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	for range 3 {
		_, err = restarted.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	run, err = restarted.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunUncertain, run.Run.State)
	require.Len(t, provider.runtimes[plan.ExecutionID].inputs, 1)
}

type stageInteractionCompletionFailure struct {
	*sqlite.Store
	fail bool
}

func (s *stageInteractionCompletionFailure) CompleteOperation(ctx context.Context, in app.OperationCompletion) (app.AdmissionResult, error) {
	if s.fail && in.ResultCode == "work_input_accepted" {
		return app.AdmissionResult{}, context.Canceled
	}
	return s.Store.CompleteOperation(ctx, in)
}

func TestTaskStagesReusedInputRechecksAuthorityBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	provider := &stageAgentProvider{runtimes: map[model.ExecutionID]*stageAgentRuntime{}}
	wrapped := &stageInteractionClaimHook{Store: store}
	service := app.New(wrapped, providers.NewRegistry(provider))
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	worker, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "caller", Name: "Caller", Desired: desired})
	require.NoError(t, err)
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "launch"}, Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: worker.Agent.ID, ExpectedRevision: worker.Agent.Revision}}, InitialMessage: "Original work"})
	require.NoError(t, err)
	for _, grant := range []model.AuthorityGrant{
		{ID: "start", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionStartWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: "run"}},
		{ID: "interact", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionInteract, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID}},
	} {
		_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: model.OperatorPrincipal(), Grant: grant})
		require.NoError(t, err)
	}
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.Plan = nil
	graph.Nodes[0].Stages.PlanApproval = nil
	graph.Nodes[0].Stages.Checks = nil
	graph.Nodes[0].Retry = model.RetryPolicy{}
	graph.Nodes[0].Performer = &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", ContextPolicy: model.AgentContextReuse, Brief: "Next work"}}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.AgentPrincipal("caller"), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	wrapped.before = func() {
		require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: model.OperatorPrincipal(), GrantID: "interact", ExpectedRevision: 1}))
	}
	_, err = service.ReconcilePendingWork(ctx)

	require.NoError(t, err)
	run, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunFailed, run.Run.State)
	require.Empty(t, provider.runtimes[launched.Execution.ID].inputs)
	require.Len(t, provider.requests, 1)
}

type stageInteractionClaimHook struct {
	*sqlite.Store
	before func()
}

func (s *stageInteractionClaimHook) ClaimGraphInteraction(ctx context.Context, id model.OperationID, owner string, at time.Time) (bool, error) {
	if s.before != nil {
		f := s.before
		s.before = nil
		f()
	}
	return s.Store.ClaimGraphInteraction(ctx, id, owner, at)
}
