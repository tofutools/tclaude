package app

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
	"time"
)

func TestJoinAllRechecksWhenLastCandidateRouteIsSkipped(t *testing.T) {
	graph := model.WorkGraph{EntryNodeID: "fork", Nodes: []model.WorkNode{{ID: "fork", Kind: model.WorkNodeFork}, {ID: "early", Kind: model.WorkNodeTask}, {ID: "choose", Kind: model.WorkNodeDecision}, {ID: "join", Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}}, {ID: "done", Kind: model.WorkNodeEnd}, {ID: "other", Kind: model.WorkNodeEnd}}, Edges: []model.WorkEdge{{From: "fork", To: "early"}, {From: "fork", To: "choose"}, {From: "early", To: "join"}, {From: "choose", To: "join", Verdict: "yes"}, {From: "choose", To: "other", Verdict: "no"}, {From: "join", To: "done"}}}
	for _, earlyState := range []model.WorkNodeAttemptState{model.NodeAttemptSucceeded, model.NodeAttemptWaiting} {
		t.Run(string(earlyState), func(t *testing.T) {
			attempt := func(id model.WorkNodeID, state model.WorkNodeAttemptState) model.WorkNodeAttempt {
				return model.WorkNodeAttempt{Ref: model.WorkAttemptRef{RunID: "run", NodeID: id, ActivationID: model.WorkActivationID(id), Attempt: 1, IssuanceID: "issued"}, State: state}
			}
			current := attempt("choose", model.NodeAttemptWaiting)
			record := WorkRunRecord{Run: model.WorkRun{ID: "run", Revision: 1, Graph: &graph, Deadline: time.Now().Add(time.Hour), NodeAttempts: []model.WorkNodeAttempt{attempt("fork", model.NodeAttemptSucceeded), attempt("early", earlyState), current}}}
			service := &Service{now: time.Now, newID: func(prefix string) string { return prefix + "test" }}
			transition := service.graphOutcomeTransitionForVerdict(record, current, model.WorkOutcomeVerified, "decided", "no")
			count := 0
			for _, a := range transition.Activations {
				if a.Ref.NodeID == "join" {
					count++
				}
			}
			if earlyState == model.NodeAttemptSucceeded {
				require.Equal(t, 1, count, "a skipped final candidate must release an already-arrived join")
			} else {
				require.Zero(t, count, "skip evidence cannot substitute for a pending arrival")
			}
		})
	}
}
