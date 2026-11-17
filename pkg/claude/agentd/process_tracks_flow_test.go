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

// Tracks: several loops that start at their decision run alongside each other
// and join before the run continues. Each track here is a reviewer agent that
// decides, and the work it sends back goes to the implementer (sameAs), so a
// track's rounds run at their own pace and the join waits for every track.

// reviewTracksTemplate is implement → tracks ⫴ {review-a ↺ fix-a, review-b ↺
// fix-b} → signoff (join: all) → done.
func reviewTracksTemplate(id string, rounds int) *model.Template {
	review := func(role, fix string) model.Node {
		return model.Node{Type: model.NodeTypeDecision,
			Performer: &model.Performer{Kind: model.PerformerAgent, Role: role, Prompt: "Review the change and decide."},
			Next:      model.Next{"approved": "signoff", "changes": fix},
			Loops:     map[string]model.LoopBound{"changes": {MaxIterations: rounds}}}
	}
	fix := func(review string) model.Node {
		return model.Node{Type: model.NodeTypeTask, Next: model.Next{"next": review},
			Performer: &model.Performer{Kind: model.PerformerAgent, SameAs: "implement", Prompt: "Address the review."}}
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"next": "tracks"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Implement the feature."}},
			"tracks":   {Type: model.NodeTypeParallel, Next: model.Next{"a": "review-a", "b": "review-b"}},
			"review-a": review("reviewer-a", "fix-a"),
			"fix-a":    fix("review-a"),
			"review-b": review("reviewer-b", "fix-b"),
			"fix-b":    fix("review-b"),
			"signoff": {Type: model.NodeTypeTask, Join: model.JoinAll, Next: model.Next{"next": "done"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Role: "lead", Prompt: "Sign off."}},
			"done": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

func reportTrackVerdict(t *testing.T, f *testharness.Flow, conv, runID, node string, attempt int, verdict, note string) {
	t.Helper()
	rec := reportAs(t, f, conv, map[string]any{
		"runId": runID, "nodeId": node, "attempt": attempt, "outcome": "succeeded", "verdict": verdict, "note": note,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func commandNodeIDs(view loopRunView) []string {
	ids := make([]string, 0, len(view.Commands))
	for _, command := range view.Commands {
		ids = append(ids, command.NodeID)
	}
	return ids
}

// Scenario: two reviewer tracks run alongside each other. Reviewer a asks for
// changes twice and reviewer b approves at once; the fixes go back to the
// implementer, and sign-off waits until both tracks have approved.
func TestProcessTracks_JoinWaitsForEveryTrack(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const dev, revA, revB, lead = "tdev-1111-2222-3333-4444", "tra1-1111-2222-3333-4444",
		"trb1-1111-2222-3333-4444", "tlea-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", dev, "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", revA, "ann", "reviewer-a", "idle")
	havePerformerAgent(t, f, "crew", revB, "bob", "reviewer-b", "idle")
	havePerformerAgent(t, f, "crew", lead, "lea", "lead", "idle")
	const runID = "run_review_tracks"
	startLoopRun(t, f, root, runID, reviewTracksTemplate("review-tracks", 3))

	reportLoopStep(t, f, dev, runID, "implement", 1)
	view := showLoopRun(t, f, runID)
	assert.ElementsMatch(t, []string{"review-a", "review-b"}, commandNodeIDs(view), "both tracks start alongside each other")

	// Track a sends the work back; track b approves meanwhile.
	reportTrackVerdict(t, f, revA, runID, "review-a", 1, "changes", "handle the empty input")
	reportTrackVerdict(t, f, revB, runID, "review-b", 1, "approved", "looks good")
	view = showLoopRun(t, f, runID)
	assert.Equal(t, []string{"fix-a"}, commandNodeIDs(view), "sign-off waits for track a")
	performer := showPerformerRun(t, f, runID)
	require.NotNil(t, performer.Commands[0].Performer)
	assert.Equal(t, alice, performer.Commands[0].Performer.AgentID, "the fix goes to the implementer")
	inbox := processInbox(t, dev)
	require.NotEmpty(t, inbox)
	assert.Contains(t, inbox[len(inbox)-1].Body, "handle the empty input", "the reviewer's reasoning travels with the round")

	reportLoopStep(t, f, dev, runID, "fix-a", 1)
	view = showLoopRun(t, f, runID)
	assert.Equal(t, []string{"review-a"}, commandNodeIDs(view))
	require.Len(t, view.Commands, 1)
	assert.Equal(t, 2, view.Commands[0].Round)

	// A second round of changes; the join still waits.
	reportTrackVerdict(t, f, revA, runID, "review-a", 2, "changes", "rename the flag")
	reportLoopStep(t, f, dev, runID, "fix-a", 2)
	view = showLoopRun(t, f, runID)
	assert.Equal(t, []string{"review-a"}, commandNodeIDs(view), "sign-off has not started")
	assert.Equal(t, 3, view.Commands[0].Round)
	for _, loop := range view.Loops {
		if loop.From == "review-a" {
			assert.Equal(t, 3, loop.Round)
		} else {
			assert.Equal(t, 1, loop.Round, loop.From)
		}
	}

	reportTrackVerdict(t, f, revA, runID, "review-a", 3, "approved", "good now")
	view = showLoopRun(t, f, runID)
	assert.Equal(t, []string{"signoff"}, commandNodeIDs(view), "both tracks approved: the run goes on")
	reportLoopStep(t, f, lead, runID, "signoff", 1)
	assert.Equal(t, engine.RunCompleted, showLoopRun(t, f, runID).Status)

	kinds, _ := performerEventKinds(t, f, runID)
	assert.Equal(t, 2, countKind(kinds, "loop_iterated"))
}
