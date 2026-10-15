package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-eu70x9: bounded review loops. A decision outcome that goes back to
// earlier work runs that work again, round by round, up to its limit. These
// flows drive the canonical shape through the daemon mux with simulated
// agents: implement (dev) → review (reviewer) → decide (human), where
// "changes" goes back to implement for at most N rounds and a used-up loop
// escalates.

// reviewLoopFlowTemplate is implement → review → decide {approved → done,
// changes ↺ implement (max rounds), escalate → escalated}.
func reviewLoopFlowTemplate(id string, rounds int) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"next": "review"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Implement the feature."}},
			"review": {Type: model.NodeTypeTask, Next: model.Next{"next": "decide"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review the change."}},
			"decide": {Type: model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"},
				Next:      model.Next{"approved": "done", "changes": "implement", "escalate": "escalated"},
				Loops:     map[string]model.LoopBound{"changes": {MaxIterations: rounds, OnExhausted: "escalate"}}},
			"done":      {Type: model.NodeTypeEnd, Result: "success"},
			"escalated": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
}

// startLoopRun creates the run in the group's persisted scope, so it also
// shows in the process context.
func startLoopRun(t *testing.T, f *testharness.Flow, root, runID string, tmpl *model.Template) {
	t.Helper()
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
}

func decideLoop(t *testing.T, f *testharness.Flow, runID string, body map[string]any) int {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+runID+"/decide", body)
	agentd.WaitForProcessDispatchForTest()
	return rec.Code
}

func reportLoopStep(t *testing.T, f *testharness.Flow, conv, runID, node string, attempt int) {
	t.Helper()
	rec := reportAs(t, f, conv, map[string]any{
		"runId": runID, "nodeId": node, "attempt": attempt, "outcome": "succeeded", "note": node + " done",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

type loopRunView struct {
	Status engine.RunStatus `json:"status"`
	Loops  []struct {
		From          string `json:"from"`
		Outcome       string `json:"outcome"`
		MaxIterations int    `json:"maxIterations"`
		Round         int    `json:"round"`
	} `json:"loops"`
	AwaitingDecisions []struct {
		NodeID  string `json:"nodeId"`
		Attempt int    `json:"attempt"`
	} `json:"awaitingDecisions"`
	Commands []struct {
		NodeID  string `json:"nodeId"`
		Attempt int    `json:"attempt"`
		Round   int    `json:"round"`
		Rounds  int    `json:"rounds"`
	} `json:"commands"`
}

func showLoopRun(t *testing.T, f *testharness.Flow, runID string) loopRunView {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view loopRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

// Scenario: review sends the work back twice with feedback, then approves.
// Each round goes back to the same implementer, who is told the round and
// the reviewer's feedback; the run view, the context, and the evidence
// record every round, and a verdict formed in an earlier round is refused.
func TestProcessLoop_ReviewChangesTwiceThenApproved(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev, reviewer = "ldev-1111-2222-3333-4444", "lrev-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", reviewer, "carol", "reviewer", "idle")
	const runID = "run_review_loop"
	startLoopRun(t, f, root, runID, reviewLoopFlowTemplate("review-loop", 3))

	reportLoopStep(t, f, dev, runID, "implement", 1)
	reportLoopStep(t, f, reviewer, runID, "review", 1)
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{
		"nodeId": "decide", "verdict": "changes", "evidence": "add tests for the edge case"}))

	view := showLoopRun(t, f, runID)
	require.Len(t, view.Commands, 1)
	assert.Equal(t, "implement", view.Commands[0].NodeID)
	assert.Equal(t, 2, view.Commands[0].Attempt)
	assert.Equal(t, 2, view.Commands[0].Round)
	assert.Equal(t, 3, view.Commands[0].Rounds)
	require.Len(t, view.Loops, 1)
	assert.Equal(t, 2, view.Loops[0].Round)
	assert.Equal(t, 3, view.Loops[0].MaxIterations)

	performer := showPerformerRun(t, f, runID)
	require.NotNil(t, performer.Commands[0].Performer)
	assert.Equal(t, alice, performer.Commands[0].Performer.AgentID, "the next round goes back to the implementer")
	assert.Equal(t, "same_session", performer.Commands[0].Performer.Resolution)
	inbox := processInbox(t, dev)
	require.Len(t, inbox, 2)
	assert.Contains(t, inbox[1].Body, "Round:   2 of at most 3")
	assert.Contains(t, inbox[1].Body, "add tests for the edge case", "the reviewer's feedback travels with the round")

	// The dashboard reads rounds and the back edge's limit off the context.
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var context struct {
		Runs map[string]struct {
			Nodes []struct {
				ID     string `json:"id"`
				Round  int    `json:"round"`
				Rounds int    `json:"rounds"`
			} `json:"nodes"`
			Edges []struct {
				From          string `json:"from"`
				Label         string `json:"label"`
				Back          bool   `json:"back"`
				MaxIterations int    `json:"maxIterations"`
			} `json:"edges"`
		} `json:"runs"`
	}
	testharness.DecodeJSON(t, rec, &context)
	projected, ok := context.Runs[runID]
	require.True(t, ok)
	for _, node := range projected.Nodes {
		switch node.ID {
		case "implement", "review", "decide":
			assert.Equal(t, [2]int{2, 3}, [2]int{node.Round, node.Rounds}, node.ID)
		default:
			assert.Zero(t, node.Round, node.ID)
		}
	}
	backEdges := 0
	for _, edge := range projected.Edges {
		if edge.MaxIterations > 0 {
			backEdges++
			assert.Equal(t, "decide/changes", edge.From+"/"+edge.Label)
			assert.True(t, edge.Back)
			assert.Equal(t, 3, edge.MaxIterations)
		}
	}
	assert.Equal(t, 1, backEdges)

	reportLoopStep(t, f, dev, runID, "implement", 2)
	reportLoopStep(t, f, reviewer, runID, "review", 2)
	view = showLoopRun(t, f, runID)
	require.Len(t, view.AwaitingDecisions, 1)
	assert.Equal(t, 1, view.AwaitingDecisions[0].Attempt, "round two's decision is a new window")
	assert.Equal(t, http.StatusConflict, decideLoop(t, f, runID, map[string]any{
		"nodeId": "decide", "verdict": "changes", "attempt": 0}), "a verdict for round one is stale")
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{
		"nodeId": "decide", "verdict": "changes", "attempt": 1, "evidence": "rename the flag"}))

	reportLoopStep(t, f, dev, runID, "implement", 3)
	reportLoopStep(t, f, reviewer, runID, "review", 3)
	inbox = processInbox(t, dev)
	require.Len(t, inbox, 3)
	assert.Contains(t, inbox[2].Body, "Round:   3 of at most 3")
	assert.Contains(t, inbox[2].Body, "rename the flag")
	require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{
		"nodeId": "decide", "verdict": "approved", "evidence": "ship it"}))

	assert.Equal(t, engine.RunCompleted, showLoopRun(t, f, runID).Status)
	kinds, events := performerEventKinds(t, f, runID)
	assert.Equal(t, 2, countKind(kinds, "loop_iterated"))
	var rounds []int
	for _, event := range events {
		if event.Kind != "loop_iterated" {
			continue
		}
		var payload struct {
			Round    int    `json:"round"`
			Feedback string `json:"feedback"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		rounds = append(rounds, payload.Round)
	}
	assert.Equal(t, []int{2, 3}, rounds)
}

// Scenario: the reviewer keeps asking for changes until the loop's rounds are
// used up; the last "changes" follows onExhausted and escalates the run.
func TestProcessLoop_ExhaustedLoopEscalates(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev, reviewer = "edev-1111-2222-3333-4444", "erev-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", reviewer, "carol", "reviewer", "idle")
	const runID = "run_loop_exhausted"
	startLoopRun(t, f, root, runID, reviewLoopFlowTemplate("loop-exhausted", 2))

	for round := 1; round <= 2; round++ {
		reportLoopStep(t, f, dev, runID, "implement", round)
		reportLoopStep(t, f, reviewer, runID, "review", round)
		require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{
			"nodeId": "decide", "verdict": "changes", "evidence": "still not right"}))
	}
	assert.Equal(t, engine.RunCanceled, showLoopRun(t, f, runID).Status)
	kinds, events := performerEventKinds(t, f, runID)
	assert.Equal(t, 1, countKind(kinds, "loop_iterated"))
	require.Equal(t, 1, countKind(kinds, "loop_exhausted"))
	for _, event := range events {
		if event.Kind == "decision_recorded" {
			var payload struct {
				ChosenEdge *engine.ChosenEdge `json:"chosenEdge"`
			}
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			require.NotNil(t, payload.ChosenEdge)
		}
	}
	last := events[len(events)-1]
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "decision_recorded" {
			last = events[i]
			break
		}
	}
	var payload struct {
		ChosenEdge engine.ChosenEdge `json:"chosenEdge"`
	}
	require.NoError(t, json.Unmarshal(last.Payload, &payload))
	assert.Equal(t, engine.ChosenEdge{From: "decide", Outcome: "escalate", To: "escalated"}, payload.ChosenEdge,
		"a used-up loop follows onExhausted")
}
