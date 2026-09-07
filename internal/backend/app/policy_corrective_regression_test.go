package app_test

import (
	"context"
	"fmt"
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

func TestHumanRejectionRetryCreatesNewDecisionWindow(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := waitingDecisionGraph()
	graph.Nodes[0].Decision.PermittedAnswers = []string{"approve", "reject"}
	graph.Nodes[0].Retry = model.RetryPolicy{MaxAttempts: 2, Retryable: []string{model.RetryableHumanRejection}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "human_retry", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "reject"}, DecisionID: run.Decisions[0].ID, ExpectedWindowRevision: run.Decisions[0].Revision, Answer: "reject", Reason: "try again"})
	require.NoError(t, err)
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: run.Run.ID})
	require.NoError(t, err)
	retry := attemptFor(t, run, "approve", 2)
	require.Equal(t, model.NodeAttemptWaiting, retry.State)
	require.NotEmpty(t, retry.DecisionID)
	require.Len(t, run.Decisions, 2)
	require.Equal(t, retry.Ref, decisionByID(t, run, retry.DecisionID).Attempt)
}

func TestHumanRetryBackoffOpensDecisionOnlyWhenEligible(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := waitingDecisionGraph()
	graph.Nodes[0].Decision.PermittedAnswers = []string{"approve", "reject"}
	graph.Nodes[0].Retry = model.RetryPolicy{MaxAttempts: 2, Backoff: time.Minute, Retryable: []string{model.RetryableHumanRejection}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "human_backoff", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "reject"}, DecisionID: run.Decisions[0].ID, ExpectedWindowRevision: run.Decisions[0].Revision, Answer: "reject", Reason: "try later"})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	parked, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptRetryWait, attemptFor(t, parked, "approve", 2).State)
	require.Len(t, parked.Decisions, 1)
	now = now.Add(time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	ready, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: run.Run.ID})
	require.NoError(t, err)
	retry := attemptFor(t, ready, "approve", 2)
	require.Equal(t, model.NodeAttemptWaiting, retry.State)
	require.NotEmpty(t, retry.DecisionID)
	require.Len(t, ready.Decisions, 2)
}

func TestBlockedHumanRetryCreatesExactDecisionWindow(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := waitingDecisionGraph()
	graph.Nodes[0].Decision.PermittedAnswers = []string{"approve", "reject"}
	graph.Nodes[0].Retry = model.RetryPolicy{MaxAttempts: 1, Retryable: []string{model.RetryableHumanRejection}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "human_blocked", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "reject"}, DecisionID: run.Decisions[0].ID, ExpectedWindowRevision: run.Decisions[0].Revision, Answer: "reject", Reason: "needs rework"})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	blocked, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: run.Run.ID})
	require.NoError(t, err)
	attempt := attemptFor(t, blocked, "approve", 1)
	require.Equal(t, model.NodeAttemptBlocked, attempt.State)
	require.Len(t, blocked.Decisions, 2)
	blockedWindow := decisionByID(t, blocked, attempt.DecisionID)
	resolved, err := service.ResolveBlocked(ctx, app.ResolveBlockedRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "retry"}, DecisionID: blockedWindow.ID, Attempt: attempt.Ref, ExpectedWindowRevision: blockedWindow.Revision, ExpectedRunRevision: blocked.Run.Revision, Action: model.BlockedRetry, Reason: "operator requests exact retry"})
	require.NoError(t, err)
	retry := attemptFor(t, resolved, "approve", 2)
	require.Equal(t, model.NodeAttemptWaiting, retry.State)
	require.NotEmpty(t, retry.DecisionID)
	require.Len(t, resolved.Decisions, 3)
	require.Equal(t, retry.Ref, decisionByID(t, resolved, retry.DecisionID).Attempt)
}

func decisionByID(t *testing.T, run app.WorkRunResult, id model.DecisionID) model.DecisionWindow {
	t.Helper()
	for _, decision := range run.Decisions {
		if decision.ID == id {
			return decision
		}
	}
	t.Fatalf("missing decision %s", id)
	return model.DecisionWindow{}
}

func TestAutomationConditionRevisionResetsPersistedCursor(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	req := correctiveTriggerRule(now)
	saved, err := service.SaveAutomationRule(ctx, req)
	require.NoError(t, err)
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, "github", []model.NormalizedFact{{EventID: "event", Resource: req.Condition.Trigger.Resource, Kind: model.FactCICompleted, Value: "succeeded", OccurredAt: now, ObservedAt: now}}))
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	req.Context.RequestID, req.RevisionID, req.ExpectedRevision = "edit", "v2", saved.Rule.Revision
	req.Condition = model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now}}
	_, err = service.SaveAutomationRule(ctx, req)
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
}

func TestTriggerCatchUpAppliesNegativeBeforeDebounceDispatch(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	req := correctiveTriggerRule(now)
	_, err = service.SaveAutomationRule(ctx, req)
	require.NoError(t, err)
	facts := []model.NormalizedFact{
		{EventID: "positive", Resource: req.Condition.Trigger.Resource, Kind: model.FactCICompleted, Value: "succeeded", OccurredAt: now, ObservedAt: now},
		{EventID: "negative", Resource: req.Condition.Trigger.Resource, Kind: model.FactCICompleted, Value: "failed", OccurredAt: now.Add(30 * time.Second), ObservedAt: now.Add(30 * time.Second)},
	}
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, "github", facts))
	now = now.Add(2 * time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: req.ID})
	require.NoError(t, err)
	require.Empty(t, occurrences)
}

func TestTriggerFullPagePersistsContinuationBeforeWallClockDispatch(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	req := correctiveTriggerRule(now)
	_, err = service.SaveAutomationRule(ctx, req)
	require.NoError(t, err)
	firstPage := make([]model.NormalizedFact, 0, 256)
	firstPage = append(firstPage, model.NormalizedFact{EventID: "positive", Resource: req.Condition.Trigger.Resource, Kind: model.FactCICompleted, Value: "succeeded", OccurredAt: now, ObservedAt: now})
	for i := 1; i < 256; i++ {
		observed := now.Add(time.Duration(i) * time.Millisecond)
		firstPage = append(firstPage, model.NormalizedFact{EventID: fmt.Sprintf("irrelevant-%03d", i), Resource: req.Condition.Trigger.Resource, Kind: model.FactPullRequestChanged, Value: "open", OccurredAt: observed, ObservedAt: observed})
	}
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, "github", firstPage))
	negativeAt := now.Add(30 * time.Second)
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, "github", []model.NormalizedFact{{EventID: "negative", Resource: req.Condition.Trigger.Resource, Kind: model.FactCICompleted, Value: "failed", OccurredAt: negativeAt, ObservedAt: negativeAt}}))
	now = now.Add(2 * time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: req.ID})
	require.NoError(t, err)
	require.Empty(t, occurrences, "a full page must not speculate past unread evidence")
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err = service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: req.ID})
	require.NoError(t, err)
	require.Empty(t, occurrences, "the persisted continuation must consume the queued negative evidence")
}

func correctiveTriggerRule(now time.Time) app.SaveAutomationRuleRequest {
	graph := waitingDecisionGraph()
	return app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "v1", Name: "review", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: automationWorkDelegation(now, "rule"), Condition: model.AutomationCondition{Kind: model.AutomationTrigger, Trigger: &model.TriggerCondition{SourceID: "github", Resource: model.AutomationFactResource{Kind: model.FactResourceRepositoryPullReq, Repository: "test/repo", PullRequest: 1}, FactKind: model.FactCICompleted, Values: []string{"succeeded"}, Freshness: time.Hour, Debounce: time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}, Policy: occurrencePolicy(model.MissedTickCoalesce, model.OverlapAllow, 10)}
}

type cancellationDuringObserveHost struct {
	programHostFake
	service   *app.Service
	cancelled bool
}

func (h *cancellationDuringObserveHost) PrepareProgram(ctx context.Context, request ports.ProgramPreparationRequest) (ports.PreparedProgram, error) {
	prepared, err := h.programHostFake.PrepareProgram(ctx, request)
	return &cancellationDuringObservePrepared{PreparedProgram: prepared, host: h, runID: request.WorkspaceUse.WorkRunID}, err
}

type cancellationDuringObservePrepared struct {
	ports.PreparedProgram
	host  *cancellationDuringObserveHost
	runID model.WorkRunID
}

func (p *cancellationDuringObservePrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ProgramReleaseResult, error) {
	result, err := p.PreparedProgram.Release(ctx, permit)
	result.Runtime = &cancellationDuringObserveRuntime{ProgramRuntime: result.Runtime, host: p.host, runID: p.runID}
	return result, err
}

type cancellationDuringObserveRuntime struct {
	ports.ProgramRuntime
	host  *cancellationDuringObserveHost
	runID model.WorkRunID
}

func (r *cancellationDuringObserveRuntime) ObserveProgram(ctx context.Context) (ports.ProgramObservation, error) {
	if !r.host.cancelled {
		run, err := r.host.service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: r.runID})
		if err != nil {
			return ports.ProgramObservation{}, err
		}
		_, err = r.host.service.CancelWork(ctx, app.CancelWorkRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "cancel"}, WorkRunID: r.runID, ExpectedRunRevision: run.Run.Revision, Reason: "operator cancellation"})
		if err != nil {
			return ports.ProgramObservation{}, err
		}
		r.host.cancelled = true
	}
	return r.ProgramRuntime.ObserveProgram(ctx)
}

func TestCancellationSettlesBeforeRetryPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	host := &cancellationDuringObserveHost{programHostFake: programHostFake{exitCodes: []int{1, 0}}}
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithProgramHost(host)
	host.service = service
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	ref := model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: ref}}, Retry: model.RetryPolicy{MaxAttempts: 2, Retryable: []string{model.RetryableProgramFailure}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "cancelled", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{ref}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	for range 3 {
		_, _ = service.ReconcilePendingWork(ctx)
	}
	settled, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkControlSettled, settled.Run.ControlState)
	require.Len(t, settled.Run.NodeAttempts, 1)
}

func TestUnavailableRunDoesNotStarveLaterIndependentRun(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	host := &programHostFake{}
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithProgramHost(host)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "offline", Name: "offline", Desired: model.DesiredConfiguration{Harness: "unregistered", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	agentGraph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "agent", Nodes: []model.WorkNode{{ID: "agent", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "offline", Brief: "work"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "agent", To: "done"}}}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "offline-start"}, ID: "offline_run", Start: model.WorkStart{InlineGraph: &agentGraph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	now = now.Add(time.Nanosecond)
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "program", RevisionID: "program_v1", Name: "program", Executable: "program", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	graph := programGraph(profile)
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "program-start"}, ID: "program_run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.ErrorIs(t, err, app.ErrUnavailable)
	require.Equal(t, 1, host.prepares)
	offline, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "offline_run"})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptReady, attemptFor(t, offline, "agent", 1).State)
	require.Contains(t, attemptFor(t, offline, "agent", 1).Detail, "has no provider")
	require.Empty(t, attemptFor(t, offline, "agent", 1).Ref.IssuanceID)
}
