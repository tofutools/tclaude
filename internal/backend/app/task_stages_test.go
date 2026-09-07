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
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func stagedHumanGraph() model.WorkGraph {
	human := model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true, Prompt: "Perform this stage"}}
	return model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{
		{ID: "task", Name: "Deliver change", Kind: model.WorkNodeTask, Performer: &human, Retry: model.RetryPolicy{MaxAttempts: 2, Retryable: []string{model.RetryableHumanRejection}}, Stages: &model.TaskStages{
			Plan:         &model.TaskStage{ID: "plan", Name: "Plan", Performer: human},
			PlanApproval: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve", "rework"}, ExpiresAfter: time.Hour},
			Checks:       []model.TaskStage{{ID: "check", Name: "Check", Performer: human}, {ID: "check_two", Name: "Second check", Performer: human}}, Review: &model.TaskStage{ID: "review", Name: "Review", Performer: human},
		}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "task", To: "done"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"task"}}}
}

func TestTaskStagesPlanReworkAndFailedChecksSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stages.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := stagedHumanGraph()
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "staged_run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}
	run, err := service.StartProcess(ctx, start)
	require.NoError(t, err)
	require.Len(t, run.Run.Graph.TaskGroups, 1)
	group := run.Run.Graph.TaskGroups[0]
	require.Equal(t, model.WorkNodeTaskComplete, run.Run.Graph.Nodes[len(run.Run.Graph.Nodes)-2].Kind)
	sequence := 0
	answer := func(node model.WorkNodeID, value string) model.WorkNodeAttempt {
		t.Helper()
		run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: start.ID})
		require.NoError(t, err)
		var current model.WorkNodeAttempt
		for _, a := range run.Run.NodeAttempts {
			if a.Ref.NodeID == node && a.State == model.NodeAttemptWaiting {
				current = a
			}
		}
		require.NotEmpty(t, current.DecisionID, "stage %s must be waiting", node)
		decision := decisionByID(t, run, current.DecisionID)
		sequence++
		_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID(fmt.Sprintf("answer_%d", sequence))}, DecisionID: decision.ID, ExpectedWindowRevision: decision.Revision, Answer: value, Reason: "stage feedback"})
		require.NoError(t, err)
		return current
	}
	answer(group.Plan, "complete")
	firstApproval := answer(group.Approval, "rework")
	answer(group.Plan, "complete")
	secondApproval := answer(group.Approval, "approve")
	require.NotEqual(t, firstApproval.DecisionID, secondApproval.DecisionID)
	firstWork := answer(group.Work, "complete")
	answer(group.Checks[0], "complete")
	failed := answer(group.Checks[1], "reject")
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	secondWork := answer(group.Work, "complete")
	require.Equal(t, firstWork.Ref.ActivationID, secondWork.Ref.ActivationID)
	require.Greater(t, secondWork.Ref.Attempt, failed.Ref.Attempt)
	answer(group.Checks[0], "complete")
	answer(group.Checks[1], "complete")
	answer(group.Review, "complete")
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: start.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, run.Run.State)
	require.Equal(t, model.NodeAttemptFailed, attemptFor(t, run, failed.Ref.NodeID, failed.Ref.Attempt).State, "failed proof is retained")
	var plans, works, completions int
	for _, a := range run.Run.NodeAttempts {
		switch a.Ref.NodeID {
		case group.Plan:
			plans++
		case group.Work:
			works++
		case group.ID:
			completions++
		}
	}
	require.Equal(t, 2, plans, "check feedback must retain the approved plan")
	require.Equal(t, 2, works, "human plan rework does not consume the work budget")
	require.Equal(t, 1, completions)
	repeated, err := service.StartProcess(ctx, start)
	require.NoError(t, err)
	require.Equal(t, run.Run.NodeAttempts, repeated.Run.NodeAttempts, "exact start does not replay stages")
}

func TestTaskStagesRejectForgedCompilerGroupsAndInvalidStages(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	cases := []struct {
		name   string
		mutate func(*model.WorkGraph)
	}{
		{"forged group", func(g *model.WorkGraph) { g.TaskGroups = []model.CompiledTaskGroup{{ID: "task", Work: "task"}} }},
		{"forged completion", func(g *model.WorkGraph) { g.Nodes[0].Kind = model.WorkNodeTaskComplete; g.Nodes[0].Stages = nil }},
		{"duplicate stage", func(g *model.WorkGraph) { g.Nodes[0].Stages.Checks[0].ID = "plan" }},
		{"approval without plan", func(g *model.WorkGraph) { g.Nodes[0].Stages.Plan = nil }},
		{"gate retry", func(g *model.WorkGraph) { g.Nodes[0].Stages.Checks[0].Retry.MaxAttempts = 5 }},
		{"unknown approval answer", func(g *model.WorkGraph) {
			g.Nodes[0].Stages.PlanApproval.PermittedAnswers = []string{"approve", "ignore"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := stagedHumanGraph()
			tc.mutate(&g)
			_, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &g, Deadline: time.Now().Add(time.Hour)}})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
}

func TestTaskStagesExhaustedReviewResolutionReplaysOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stages.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.Plan = nil
	graph.Nodes[0].Stages.PlanApproval = nil
	graph.Nodes[0].Stages.Checks = nil
	graph.Nodes[0].Retry.MaxAttempts = 1
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	group := run.Run.Graph.TaskGroups[0]
	submit := func(answer string) {
		t.Helper()
		var window model.DecisionWindow
		for _, d := range run.Decisions {
			if d.State == model.DecisionOpen {
				window = d
			}
		}
		_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID("answer_" + string(window.ID))}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, Answer: answer, Reason: "review feedback"})
		require.NoError(t, err)
		run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
		require.NoError(t, err)
	}
	submit("complete")
	submit("reject")
	blocked := attemptFor(t, run, group.Review, 1)
	require.Equal(t, model.NodeAttemptBlocked, blocked.State)
	window := decisionByID(t, run, blocked.DecisionID)
	req := app.ResolveBlockedRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "resolve"}, DecisionID: window.ID, Attempt: blocked.Ref, ExpectedWindowRevision: window.Revision, ExpectedRunRevision: run.Run.Revision, Action: model.BlockedRework, Reason: "revise the task"}
	// Lose the response after the decision commits; no effect has been issued yet.
	_, err = store.SubmitDecision(ctx, model.DecisionSubmission{RequestID: req.Context.RequestID, DecisionID: req.DecisionID, ExpectedWindowRevision: req.ExpectedWindowRevision, ExpectedRunRevision: req.ExpectedRunRevision, Answer: string(req.Action), Reason: req.Reason, Actor: req.Context.Principal, SubmittedAt: now}, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionDecideWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: "run"}}, now)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	run, err = service.ResolveBlocked(ctx, req)
	require.NoError(t, err)
	require.Equal(t, uint32(2), attemptFor(t, run, group.Work, 2).RetryBudget)
	require.Equal(t, model.NodeAttemptWaiting, attemptFor(t, run, group.Work, 2).State)
	require.Len(t, run.Run.NodeAttempts, 3, "review failure restarts work, not review")
	submit("complete")
	submit("complete")
	require.Equal(t, model.WorkRunSucceeded, run.Run.State)
	repeated, err := service.ResolveBlocked(ctx, req)
	require.NoError(t, err)
	require.Equal(t, run.Run.NodeAttempts, repeated.Run.NodeAttempts)
	req.Reason = "different intent"
	_, err = service.ResolveBlocked(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
}

func TestTaskStagesParallelFeedbackDoesNotReplaySibling(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	a := stagedHumanGraph().Nodes[0]
	a.ID = "a"
	a.Stages.Plan = nil
	a.Stages.PlanApproval = nil
	a.Stages.Review = nil
	a.Stages.Checks = a.Stages.Checks[:1]
	b := a
	b.ID = "b"
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "fork", Nodes: []model.WorkNode{{ID: "fork", Kind: model.WorkNodeFork}, a, b, {ID: "join", Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "fork", To: "a"}, {From: "fork", To: "b"}, {From: "a", To: "join"}, {From: "b", To: "join"}, {From: "join", To: "done"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	groups := run.Run.Graph.TaskGroups
	require.Len(t, groups, 2)
	answer := func(node model.WorkNodeID, value string) {
		t.Helper()
		var current model.WorkNodeAttempt
		for _, attempt := range run.Run.NodeAttempts {
			if attempt.Ref.NodeID == node && attempt.State == model.NodeAttemptWaiting {
				current = attempt
			}
		}
		require.NotEmpty(t, current.DecisionID)
		window := decisionByID(t, run, current.DecisionID)
		_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID("answer_" + string(window.ID))}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, Answer: value, Reason: "parallel result"})
		require.NoError(t, err)
		run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
		require.NoError(t, err)
	}
	answer(groups[0].Work, "complete")
	answer(groups[1].Work, "complete")
	answer(groups[0].Checks[0], "reject")
	answer(groups[1].Checks[0], "complete")
	require.NotEqual(t, model.WorkRunSucceeded, run.Run.State)
	answer(groups[0].Work, "complete")
	answer(groups[0].Checks[0], "complete")
	require.Equal(t, model.WorkRunSucceeded, run.Run.State)
	counts := map[model.WorkNodeID]int{}
	for _, attempt := range run.Run.NodeAttempts {
		counts[attempt.Ref.NodeID]++
	}
	require.Equal(t, 2, counts[groups[0].Work])
	require.Equal(t, 1, counts[groups[1].Work])
}

func TestTaskStagesWaivedWorkDoesNotBecomeVerifiedCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.Plan = nil
	graph.Nodes[0].Stages.PlanApproval = nil
	graph.Nodes[0].Stages.Review = nil
	graph.Nodes[0].Stages.Checks = graph.Nodes[0].Stages.Checks[:1]
	graph.Nodes[0].Retry.MaxAttempts = 1
	graph.Nodes[0].Waivable = true
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	group := run.Run.Graph.TaskGroups[0]
	window := run.Decisions[0]
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "reject"}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, Answer: "reject", Reason: "cannot verify"})
	require.NoError(t, err)
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	blocked := attemptFor(t, run, group.Work, 1)
	window = decisionByID(t, run, blocked.DecisionID)
	run, err = service.ResolveBlocked(ctx, app.ResolveBlockedRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "waive"}, DecisionID: window.ID, Attempt: blocked.Ref, ExpectedWindowRevision: window.Revision, ExpectedRunRevision: run.Run.Revision, Action: model.BlockedWaive, Reason: "accept missing work"})
	require.NoError(t, err)
	check := attemptFor(t, run, group.Checks[0], 1)
	window = decisionByID(t, run, check.DecisionID)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "complete"}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, Answer: "complete", Reason: "check passed"})
	require.NoError(t, err)
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunFailed, run.Run.State)
	require.Equal(t, model.WorkOutcomeWaived, attemptFor(t, run, group.ID, 1).Outcome)
}

func TestTaskStagesNestedBindingsAndProgramAuthorityAreRequired(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "stages.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "Check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	ref := model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.Plan.Performer = model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{MemberKey: "planner", Brief: "Prepare the plan"}}
	graph.Nodes[0].Stages.Checks[0].Performer = model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: ref}}
	request := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour), PerformerBindings: map[string]model.Performer{"planner": {Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "chosen"}}}}}
	_, err = service.StartProcess(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.WorkRun(ctx, "run")
	require.ErrorIs(t, err, app.ErrNotFound, "no partial admission before nested program authorization")
	request.Start.AuthorizedProgramProfiles = []model.ProgramProfileRef{ref}
	run, err := service.StartProcess(ctx, request)
	require.NoError(t, err)
	require.Equal(t, model.AgentID("chosen"), run.Run.NodeAttempts[0].Performer.Agent.AgentID)
	require.Empty(t, run.Run.NodeAttempts[0].Performer.Agent.MemberKey)
	require.Len(t, run.Run.AuthorizedPrograms, 1)
}
