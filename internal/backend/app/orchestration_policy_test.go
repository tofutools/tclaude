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

func TestGraphRetryBackoffParksOnlyFailedBranchAndWaiverIsNotVerification(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	host := &programHostFake{exitCodes: []int{1, 1}}
	service := app.New(store, providers.NewRegistry()).WithProgramHost(host).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	performer := &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "fork", Nodes: []model.WorkNode{
		{ID: "fork", Kind: model.WorkNodeFork},
		{ID: "flaky", Kind: model.WorkNodeTask, Performer: performer, Waivable: true, Retry: model.RetryPolicy{MaxAttempts: 2, Backoff: time.Minute, Retryable: []string{model.RetryableProgramFailure}}},
		{ID: "sibling", Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Duration: time.Second}},
		{ID: "join", Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "fork", To: "flaky"}, {From: "fork", To: "sibling"}, {From: "flaky", To: "join"}, {From: "sibling", To: "join"}, {From: "join", To: "done"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"flaky"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start"}, ID: "retry_run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	between, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptRetryWait, attemptFor(t, between, "flaky", 2).State)
	now = now.Add(time.Second)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	between, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptSucceeded, attemptFor(t, between, "sibling", 1).State)
	now = now.Add(59 * time.Second)
	var parked app.WorkRunResult
	for range 4 { // wake, admit, observe, then persist the exhausted branch
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
		parked, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
		require.NoError(t, err)
		if attemptFor(t, parked, "flaky", 2).State == model.NodeAttemptBlocked {
			break
		}
	}
	blocked := attemptFor(t, parked, "flaky", 2)
	require.Equal(t, model.NodeAttemptBlocked, blocked.State)
	require.Equal(t, model.NodeAttemptSucceeded, attemptFor(t, parked, "sibling", 1).State)
	require.Len(t, parked.Decisions, 1)
	// Simulate response loss after the decision transaction commits but before
	// the application applies its graph transition. The exact request replay
	// must finish reconciliation; it must not buy a second resolution window.
	_, err = store.SubmitDecision(ctx, model.DecisionSubmission{RequestID: "waive", DecisionID: parked.Decisions[0].ID, ExpectedWindowRevision: parked.Decisions[0].Revision, ExpectedRunRevision: parked.Run.Revision, Answer: string(model.BlockedWaive), Reason: "operator accepts missing flaky branch", Actor: operator, SubmittedAt: now}, model.AuthorityRequest{Principal: operator, Action: model.ActionDecideWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: parked.Run.ID}}, now)
	require.NoError(t, err)
	resolved, err := service.ResolveBlocked(ctx, app.ResolveBlockedRequest{Context: app.RequestContext{Principal: operator, RequestID: "waive"}, DecisionID: parked.Decisions[0].ID, Attempt: blocked.Ref, ExpectedWindowRevision: parked.Decisions[0].Revision, ExpectedRunRevision: parked.Run.Revision, Action: model.BlockedWaive, Reason: "operator accepts missing flaky branch"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunFailed, resolved.Run.State, "waiver stays distinct and cannot satisfy required verification")
	require.Equal(t, model.NodeAttemptWaived, attemptFor(t, resolved, "flaky", 2).State)
}

type automationFactSourceFake struct {
	id      string
	fact    model.NormalizedFact
	failure bool
}

func (s *automationFactSourceFake) SourceID() string { return s.id }
func (s *automationFactSourceFake) CollectAutomationFacts(_ context.Context, request ports.AutomationFactCollectRequest) (ports.AutomationFactBatch, error) {
	if s.failure {
		return ports.AutomationFactBatch{}, context.DeadlineExceeded
	}
	fact := s.fact
	fact.Resource = request.Resource
	return ports.AutomationFactBatch{Facts: []model.NormalizedFact{fact}}, nil
}

func TestTrustedFactCollectionResetsFailedDwellAndFiresOncePerEpisode(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "facts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	resource := model.AutomationFactResource{Kind: model.FactResourceRepositoryPullReq, Repository: "tofutools/tclaude", PullRequest: 2436}
	source := &automationFactSourceFake{id: "github", fact: model.NormalizedFact{EventID: "check-suite:42:succeeded", Kind: model.FactCICompleted, Value: "succeeded", Resource: resource, OccurredAt: now, ObservedAt: now}}
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithAutomationFactSources(source)
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_trigger"}, ID: "ci_trigger", RevisionID: "ci_trigger_v1", Name: "CI dwell", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: automationWorkDelegation(now, "ci_trigger"), Condition: model.AutomationCondition{Kind: model.AutomationTrigger, Trigger: &model.TriggerCondition{SourceID: "github", Resource: resource, FactKind: model.FactCICompleted, Values: []string{"succeeded"}, Dwell: time.Minute, Freshness: 5 * time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: ptr(waitingDecisionGraph()), Deadline: now.Add(time.Hour)}}, Policy: occurrencePolicy(model.MissedTickCoalesce, model.OverlapForbid, 1)})
	require.NoError(t, err)

	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	now = now.Add(30 * time.Second)
	source.failure = true
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err, "a failed read is durable negative evidence, not a global scheduler failure")

	source.failure = false
	source.fact.ObservedAt = now
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	source.fact.ObservedAt = now
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "ci_trigger"})
	require.NoError(t, err)
	require.Len(t, occurrences, 1)

	now = now.Add(2 * time.Minute)
	source.fact.ObservedAt = now
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err = service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "ci_trigger"})
	require.NoError(t, err)
	require.Len(t, occurrences, 1, "continuous true snapshots do not manufacture repeated trigger edges")
}

func TestRepeatedFreshSnapshotDoesNotPoisonLaterTriggerEdges(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "edge.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC)
	resource := model.AutomationFactResource{Kind: model.FactResourceRepositoryPullReq, Repository: "tofutools/tclaude", PullRequest: 2437}
	source := &automationFactSourceFake{id: "github", fact: model.NormalizedFact{EventID: "check-suite:99:succeeded", Kind: model.FactCICompleted, Value: "succeeded", Resource: resource, OccurredAt: now, ObservedAt: now}}
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithAutomationFactSources(source)
	graph := waitingDecisionGraph()
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_edge"}, ID: "edge", RevisionID: "edge_v1", Name: "CI edge", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: automationWorkDelegation(now, "edge"), Condition: model.AutomationCondition{Kind: model.AutomationTrigger, Trigger: &model.TriggerCondition{SourceID: "github", Resource: resource, FactKind: model.FactCICompleted, Values: []string{"succeeded"}, Freshness: 5 * time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}, Policy: occurrencePolicy(model.MissedTickCoalesce, model.OverlapAllow, 3)})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	now = now.Add(time.Second)
	source.fact.ObservedAt = now
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err, "re-observing one upstream identity advances the durable cursor without duplicating its occurrence")
	source.fact.EventID = "check-suite:100:succeeded"
	source.fact.OccurredAt = now
	now = now.Add(time.Second)
	source.fact.ObservedAt = now
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "edge"})
	require.NoError(t, err)
	require.Len(t, occurrences, 2, "a later distinct upstream edge remains consumable")
}

func attemptFor(t *testing.T, run app.WorkRunResult, node model.WorkNodeID, attempt uint32) model.WorkNodeAttempt {
	t.Helper()
	for _, candidate := range run.Run.NodeAttempts {
		if candidate.Ref.NodeID == node && candidate.Ref.Attempt == attempt {
			return candidate
		}
	}
	t.Fatalf("missing attempt %s/%d", node, attempt)
	return model.WorkNodeAttempt{}
}

func TestMissedTickPoliciesPersistSkipAndCoalescedOccurrence(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := waitingDecisionGraph()
	for _, tc := range []struct {
		id     model.AutomationRuleID
		policy model.MissedTickPolicy
	}{{"skip", model.MissedTickSkip}, {"coalesce", model.MissedTickCoalesce}} {
		_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID("save_" + string(tc.id))}, ID: tc.id, RevisionID: model.AutomationRuleRevisionID(string(tc.id) + "_v1"), Name: string(tc.id), Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: automationWorkDelegation(now, tc.id), Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}, Policy: occurrencePolicy(tc.policy, model.OverlapForbid, 1)})
		require.NoError(t, err)
	}
	now = now.Add(3*time.Minute + time.Second)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	skipped, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "skip"})
	require.NoError(t, err)
	require.Empty(t, skipped)
	coalesced, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "coalesce"})
	require.NoError(t, err)
	require.Len(t, coalesced, 1)
	require.Equal(t, now.Add(-time.Second), coalesced[0].Occurrence.ScheduledAt)
}

func TestAllowOverlapReleasesParkedOccurrencesOneAtATime(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "overlap.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := waitingDecisionGraph()
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_overlap"}, ID: "overlap", RevisionID: "overlap_v1", Name: "overlap", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: automationWorkDelegation(now, "overlap"), Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}, Policy: occurrencePolicy(model.MissedTickCoalesce, model.OverlapAllow, 1)})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "overlap"})
	require.NoError(t, err)
	require.Len(t, occurrences, 3)
	require.Equal(t, model.OccurrenceAdmitted, occurrences[0].Occurrence.State)
	require.Equal(t, model.OccurrenceParked, occurrences[1].Occurrence.State)
	require.Equal(t, model.OccurrenceParked, occurrences[2].Occurrence.State)

	first, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: occurrences[0].Occurrence.WorkRunID})
	require.NoError(t, err)
	_, err = service.CancelWork(ctx, app.CancelWorkRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "cancel_first"}, WorkRunID: first.Run.ID, ExpectedRunRevision: first.Run.Revision, Reason: "release overlap slot"})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err = service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "overlap"})
	require.NoError(t, err)
	require.Equal(t, model.OccurrenceDenied, occurrences[0].Occurrence.State)
	require.Equal(t, model.OccurrenceAdmitted, occurrences[1].Occurrence.State)
	require.Equal(t, model.OccurrenceParked, occurrences[2].Occurrence.State)
}

func waitingDecisionGraph() model.WorkGraph {
	return model.WorkGraph{CompilerVersion: "1", EntryNodeID: "approve", Nodes: []model.WorkNode{{ID: "approve", Kind: model.WorkNodeDecision, Name: "approve", Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "approve", To: "done"}}}
}

func occurrencePolicy(missed model.MissedTickPolicy, overlap model.OverlapPolicy, max uint32) model.OccurrencePolicy {
	return model.OccurrencePolicy{MissedTicks: missed, OfflineDelivery: model.OfflineSkip, ExpiresAfter: time.Hour, Overlap: overlap, MaxActive: max, Deadline: time.Hour, Retry: model.RetryPolicy{MaxAttempts: 1}}
}

func automationWorkDelegation(now time.Time, rule model.AutomationRuleID) model.AutomationDelegation {
	return model.AutomationDelegation{Actions: []model.Action{model.ActionStartWork, model.ActionCancelWork}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: rule}, {Kind: model.ResourceWorkRun}}, ExpiresAt: now.Add(24 * time.Hour)}
}
