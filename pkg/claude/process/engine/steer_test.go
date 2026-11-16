package engine

import (
	"errors"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// steerTemplate: start → work (an agent step with no retry policy) → ship → end.
func steerTemplate() *model.Template {
	work := model.Node{Type: model.NodeTypeTask, Next: model.Next{model.DefaultOutcome: "ship"},
		Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Do the work."}}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "steer", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "work"}},
			"work":  work,
			"ship":  programTask("end", "ship"),
			"end":   {Type: model.NodeTypeEnd},
		},
	}
}

func steerUntilWorkIsOut(t *testing.T) (Checkpoint, *Definition, *Command) {
	t.Helper()
	definition := mustPrepare(t, steerTemplate(), nil)
	checkpoint, err := Initialize("run-steer", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "work" || command.Kind != CommandAgent {
		t.Fatalf("first command = %#v", command)
	}
	return checkpoint, definition, command
}

func mark(nodeID string, attempt int, reason string) Transition {
	return Transition{Kind: TransitionBlockMarked, Mark: &BlockMark{NodeID: nodeID, Attempt: attempt, Reason: reason}}
}

// A person parks an agent step that is out: its command is withdrawn, the
// branch waits as blocked (even without an authored retry policy), and the
// ordinary resolutions apply. Retry hands out attempt 2; skip moves on.
func TestMarkBlockedParksAnAgentStepOnTheOrdinaryResolutions(t *testing.T) {
	checkpoint, definition, command := steerUntilWorkIsOut(t)
	if _, err := Apply(checkpoint, definition, mark("work", 2, "x")); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("a mark naming another attempt: %v", err)
	}
	parked, err := Apply(checkpoint, definition, mark("work", command.Attempt, "the API it needs was removed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(parked, definition); err != nil {
		t.Fatalf("parked checkpoint: %v", err)
	}
	if parked.Nodes["work"] != NodeBlocked || len(parked.Commands) != 0 || len(parked.Blocked) != 1 || parked.Blocked[0].Reason != "the API it needs was removed" {
		t.Fatalf("parked = %#v", parked)
	}
	if _, err := Apply(parked, definition, mark("work", 1, "again")); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("marking a parked step again: %v", err)
	}

	retried, err := Apply(parked, definition, Transition{Kind: TransitionBlockedResolved, Resolution: &BlockedResolution{NodeID: "work", Attempt: 1, Action: ResolveRetry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(retried, definition); err != nil {
		t.Fatalf("retried checkpoint: %v", err)
	}
	retried, again := advanceAndPlan(t, retried, definition)
	if again == nil || again.NodeID != "work" || again.Attempt != 2 {
		t.Fatalf("after retry = %#v", again)
	}

	parked, err = Apply(retried, definition, mark("work", 2, "still broken"))
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := Apply(parked, definition, Transition{Kind: TransitionBlockedResolved, Resolution: &BlockedResolution{NodeID: "work", Attempt: 2, Action: ResolveSkip}})
	if err != nil {
		t.Fatal(err)
	}
	_, next := advanceAndPlan(t, skipped, definition)
	if next == nil || next.NodeID != "ship" {
		t.Fatalf("after skip = %#v", next)
	}
}

// Canceling gives the run up from any running state. With work out it
// drains: nothing more is planned, and it ends canceled when the last
// command comes back, even if that one succeeded.
func TestCancelRunDrainsThenEndsCanceled(t *testing.T) {
	checkpoint, definition, command := steerUntilWorkIsOut(t)
	canceled, err := Apply(checkpoint, definition, Transition{Kind: TransitionRunCanceled})
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(canceled, definition); err != nil {
		t.Fatalf("canceled checkpoint: %v", err)
	}
	if canceled.Status != RunRunning || !Draining(canceled) || len(canceled.Commands) != 1 {
		t.Fatalf("a run with work out drains: %#v", canceled)
	}
	if _, err := Apply(canceled, definition, Transition{Kind: TransitionRunCanceled}); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("canceling twice: %v", err)
	}
	if _, err := Apply(canceled, definition, mark("work", 1, "x")); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("parking in a canceled run: %v", err)
	}
	done, err := Apply(canceled, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != RunCanceled {
		t.Fatalf("drained run status = %q", done.Status)
	}
	if err := ClassifyCheckpoint(done, definition); err != nil {
		t.Fatalf("drained canceled checkpoint: %v", err)
	}

	// With nothing out (the step parked), it ends at once.
	parked, err := Apply(checkpoint, definition, mark("work", 1, "stuck"))
	if err != nil {
		t.Fatal(err)
	}
	ended, err := Apply(parked, definition, Transition{Kind: TransitionRunCanceled})
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != RunCanceled || len(ended.Blocked) != 0 {
		t.Fatalf("idle cancel = %#v", ended)
	}
	if err := ClassifyCheckpoint(ended, definition); err != nil {
		t.Fatalf("ended checkpoint: %v", err)
	}
}
