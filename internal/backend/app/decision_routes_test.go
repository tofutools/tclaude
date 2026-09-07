package app

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
	"time"
)

func TestDecisionRoutesRejectStaleAnswersAndNeverFallBackToOtherAnswers(t *testing.T) {
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "decision", Nodes: []model.WorkNode{
		{ID: "decision", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{PermittedAnswers: []string{"yes", "no"}, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, ExpiresAfter: time.Hour}},
		{ID: "accepted", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
		{ID: "rejected", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeRejected}},
	}, Edges: []model.WorkEdge{{From: "decision", To: "accepted", Verdict: "approve"}, {From: "decision", To: "rejected", Verdict: "reject"}}}
	require.ErrorIs(t, validateWorkGraph(graph), ErrInvalid)
	require.Empty(t, outgoingNodesForVerdict(graph, "decision", "yes"), "persisted stale routes must not activate unrelated branches")
	graph.Edges[0].Verdict = "yes"
	graph.Edges[1].Verdict = "no"
	require.NoError(t, validateWorkGraph(graph))
	require.Equal(t, []model.WorkNodeID{"accepted"}, outgoingNodesForVerdict(graph, "decision", "yes"))
	graph.Edges[0].Verdict = ""
	require.Equal(t, []model.WorkNodeID{"accepted"}, outgoingNodesForVerdict(graph, "decision", "yes"), "only an explicit unlabelled default may handle an unmatched answer")
}
