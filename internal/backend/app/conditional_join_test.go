package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestJoinAllWaitsForSelectedBranchesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "join.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	now := time.Date(2026, 9, 8, 6, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	human := func(id string) model.WorkNode {
		return model.WorkNode{ID: model.WorkNodeID(id), Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true, Prompt: id}}}
	}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "fork", Nodes: []model.WorkNode{
		{ID: "fork", Kind: model.WorkNodeFork},
		{ID: "choose", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"left", "right"}, ExpiresAfter: time.Hour}},
		human("left"), human("right"), human("sibling"),
		{ID: "join", Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "fork", To: "choose"}, {From: "fork", To: "sibling"}, {From: "choose", To: "left", Verdict: "left"}, {From: "choose", To: "right", Verdict: "right"}, {From: "left", To: "join"}, {From: "right", To: "join"}, {From: "sibling", To: "join"}, {From: "join", To: "done"}}}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	answer := func(node model.WorkNodeID, value string) {
		t.Helper()
		run, e := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: "run"})
		require.NoError(t, e)
		for _, window := range run.Decisions {
			if window.Attempt.NodeID == node && window.State == model.DecisionOpen {
				_, e = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: operator, RequestID: model.RequestID("answer_" + string(node))}, DecisionID: window.ID, ExpectedWindowRevision: window.Revision, ExpectedRunRevision: run.Run.Revision, Answer: value, Reason: "explicit selection"})
				require.NoError(t, e)
				return
			}
		}
		t.Fatalf("no open decision for %s", node)
	}
	answer("choose", "left")
	answer("left", "complete")
	run, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: "run"})
	require.NoError(t, err)
	for _, attempt := range run.Run.NodeAttempts {
		require.NotEqual(t, model.WorkNodeID("join"), attempt.Ref.NodeID, "a still-pending sibling must hold the join")
		require.NotEqual(t, model.WorkNodeID("right"), attempt.Ref.NodeID, "unselected work must never start")
	}
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	answer("sibling", "complete")
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, run.Run.State, "the unselected route is not an unfinished branch")
	joins := 0
	for _, attempt := range run.Run.NodeAttempts {
		if attempt.Ref.NodeID == "join" {
			joins++
		}
	}
	require.Equal(t, 1, joins)
}
