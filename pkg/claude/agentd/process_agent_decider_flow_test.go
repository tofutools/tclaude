package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-eu70x9: agent deciders. A decision whose performer is an agent is
// handed to that agent like a task; its report names the verdict, and the
// note is the reasoning the next round's performer is told.

// agentDecidedLoopTemplate is implement → decide (a reviewer agent decides)
// {approved → done, changes ↺ implement (max rounds)}, with no onExhausted:
// past the last round "changes" is refused and the reviewer has to approve.
func agentDecidedLoopTemplate(id string, rounds int) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"next": "decide"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Implement the feature."}},
			"decide": {Type: model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review the change and decide."},
				Next:      model.Next{"approved": "done", "changes": "implement"},
				Loops:     map[string]model.LoopBound{"changes": {MaxIterations: rounds}}},
			"done": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

func reportVerdict(t *testing.T, f *testharness.Flow, conv, runID string, attempt int, verdict, note string) int {
	t.Helper()
	rec := reportAs(t, f, conv, map[string]any{
		"runId": runID, "nodeId": "decide", "attempt": attempt, "outcome": "succeeded", "verdict": verdict, "note": note,
	})
	return rec.Code
}

// Scenario: the reviewer agent sends the work back once with its reasoning,
// cannot send it back past the loop's last round, and then approves.
func TestProcessAgentDecider_SendsBackThenApproves(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev, reviewer = "adev-1111-2222-3333-4444", "arev-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
	carol := havePerformerAgent(t, f, "crew", reviewer, "carol", "reviewer", "idle")
	const runID = "run_agent_decider"
	startLoopRun(t, f, root, runID, agentDecidedLoopTemplate("agent-decided-loop", 2))

	reportLoopStep(t, f, dev, runID, "implement", 1)
	view := showLoopRun(t, f, runID)
	assert.Empty(t, view.AwaitingDecisions, "an agent decision does not wait on a person")
	require.Len(t, view.Commands, 1)
	assert.Equal(t, "decide", view.Commands[0].NodeID)
	performer := showPerformerRun(t, f, runID)
	require.NotNil(t, performer.Commands[0].Performer)
	assert.Equal(t, carol, performer.Commands[0].Performer.AgentID)
	inbox := processInbox(t, reviewer)
	require.Len(t, inbox, 1)
	assert.Contains(t, inbox[0].Body, "Choose exactly one verdict: approved, changes.")
	assert.Contains(t, inbox[0].Body, "--verdict <verdict>")

	// A person cannot decide for the agent, and the agent must name a verdict
	// it was offered.
	assert.Equal(t, http.StatusConflict, decideLoop(t, f, runID, map[string]any{"nodeId": "decide", "verdict": "approved"}))
	assert.Equal(t, http.StatusUnprocessableEntity, reportVerdict(t, f, reviewer, runID, 1, "", "looks fine"))
	assert.Equal(t, http.StatusUnprocessableEntity, reportVerdict(t, f, reviewer, runID, 1, "maybe", "unsure"))

	require.Equal(t, http.StatusOK, reportVerdict(t, f, reviewer, runID, 1, "changes", "add tests for the edge case"))
	view = showLoopRun(t, f, runID)
	require.Len(t, view.Commands, 1)
	assert.Equal(t, [3]any{"implement", 2, 2}, [3]any{view.Commands[0].NodeID, view.Commands[0].Attempt, view.Commands[0].Round})
	devInbox := processInbox(t, dev)
	require.Len(t, devInbox, 2)
	assert.Contains(t, devInbox[1].Body, "add tests for the edge case", "the decider's reasoning travels with the round")
	// The same report again is acknowledged; a different verdict is a conflict.
	assert.Equal(t, http.StatusOK, reportVerdict(t, f, reviewer, runID, 1, "changes", "add tests for the edge case"))
	assert.Equal(t, http.StatusConflict, reportVerdict(t, f, reviewer, runID, 1, "approved", "add tests for the edge case"))

	reportLoopStep(t, f, dev, runID, "implement", 2)
	assert.Equal(t, http.StatusUnprocessableEntity, reportVerdict(t, f, reviewer, runID, 2, "changes", "still not right"),
		"the loop has no round left to go back for")
	require.Equal(t, http.StatusOK, reportVerdict(t, f, reviewer, runID, 2, "approved", "good now"))

	assert.Equal(t, engine.RunCompleted, showLoopRun(t, f, runID).Status)
	kinds, _ := performerEventKinds(t, f, runID)
	assert.Equal(t, 2, countKind(kinds, "decision_recorded"))
	assert.Equal(t, 1, countKind(kinds, "loop_iterated"))
}

// Scenario: an agent reviews a compound step. Its failed report sends the
// work back to the implementer, who is told which stage sent it back and why,
// in the same session; the second review passes.
func TestProcessAgentReviewStage_SendsTheWorkBackWithItsReason(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev, reviewer = "sdev-1111-2222-3333-4444", "srev-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", reviewer, "carol", "reviewer", "idle")
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "agent-reviewed", Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"next": "done"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Implement the feature."},
				Retry:     &model.RetryPolicy{MaxAttempts: 3, OnFail: model.RetryModeFeedbackSameSession},
				Review: &model.Step{ID: "review",
					Performer: model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review the change."}}},
			"done": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	const runID = "run_agent_review_stage"
	startLoopRun(t, f, root, runID, tmpl)

	reportLoopStep(t, f, dev, runID, "implement.do", 1)
	rec := reportAs(t, f, reviewer, map[string]any{
		"runId": runID, "nodeId": "implement.review", "attempt": 1, "outcome": "failed", "note": "needs tests for the empty input",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	performer := showPerformerRun(t, f, runID)
	require.Len(t, performer.Commands, 1)
	require.NotNil(t, performer.Commands[0].Performer)
	assert.Equal(t, alice, performer.Commands[0].Performer.AgentID)
	assert.Equal(t, "same_session", performer.Commands[0].Performer.Resolution)
	inbox := processInbox(t, dev)
	require.Len(t, inbox, 2)
	assert.Contains(t, inbox[1].Body, "Sent back by implement.review")
	assert.Contains(t, inbox[1].Body, "needs tests for the empty input")
	assert.NotContains(t, inbox[1].Body, "Your previous attempt (1) failed", "the implementer's own attempt did not fail")

	reportLoopStep(t, f, dev, runID, "implement.do", 2)
	reportLoopStep(t, f, reviewer, runID, "implement.review", 2)
	assert.Equal(t, engine.RunCompleted, showLoopRun(t, f, runID).Status)
	kinds, _ := performerEventKinds(t, f, runID)
	assert.Equal(t, 1, countKind(kinds, "stage_reset"))
}
