package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// Scenario: two tracks run alongside each other. Track a's reviewer agent
// gives up, which reaches a failed end; track b's agent is still at work. The
// run ends at once: b's work is withdrawn, and the run finishes failed
// without the join ever running.
func TestProcessTrackGiveUp_EndsTheRunWhileAnotherTrackWorks(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const revA, devB = "gura-1111-2222-3333-4444", "gudb-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", revA, "ann", "reviewer", "idle")
	havePerformerAgent(t, f, "crew", devB, "bob", "dev", "idle")
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "give-up-track", Start: "tracks",
		Nodes: map[string]model.Node{
			"tracks": {Type: model.NodeTypeParallel, Next: model.Next{"a": "review-a", "b": "work-b"}},
			"review-a": {Type: model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Decide."},
				Next:      model.Next{"approved": "signoff", "give-up": "gave-up"}},
			"work-b": {Type: model.NodeTypeTask, Next: model.Next{"next": "signoff"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Do the work."}},
			"signoff": {Type: model.NodeTypeTask, Join: model.JoinAll, Next: model.Next{"next": "done"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Sign off."}},
			"done":    {Type: model.NodeTypeEnd, Result: "success"},
			"gave-up": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	const runID = "run_track_give_up"
	startLoopRun(t, f, root, runID, tmpl)
	require.Len(t, showLoopRun(t, f, runID).Commands, 2)

	rec := reportAs(t, f, revA, map[string]any{
		"runId": runID, "nodeId": "review-a", "attempt": 1, "outcome": "succeeded", "verdict": "give-up", "note": "cannot approve this",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	view := showLoopRun(t, f, runID)
	assert.Equal(t, engine.RunFailed, view.Status, "the run ends without waiting for track b")
	assert.Empty(t, view.Commands, "track b's work is withdrawn")
	inbox := processInbox(t, devB)
	require.Len(t, inbox, 2)
	assert.Contains(t, inbox[1].Subject, "withdrawn", "track b's agent is told to stop")
}
