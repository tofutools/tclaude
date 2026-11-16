package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func sentBack(nodeID string) Transition {
	return Transition{Kind: TransitionSentBack, Back: &SendBack{NodeID: nodeID}}
}

// A review loop waits on its decision; the run is sent back to implement.
// implement is ready again with a fresh attempt window, everything after it
// is pending, the decision's window is dropped, and the run goes round to a
// verdict and completes as usual.
func TestSendBackResetsDownstreamAndRunsOnFromTheStep(t *testing.T) {
	definition := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 2, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-back", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 2 || checkpoint.FirstAwaitingDecision() == nil {
		t.Fatalf("setup: ran %v, awaiting %v", ran, checkpoint.AwaitingDecisions)
	}
	set, ok := definition.SendBackSet("implement")
	if !ok || !slices.Equal(set[:1], []string{"implement"}) || !slices.Contains(set, "decide") || !slices.Contains(set, "merge") || slices.Contains(set, "start") {
		t.Fatalf("send-back set = %v", set)
	}
	if _, err := Apply(checkpoint, definition, sentBack("merge")); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("sending back to a step that has not run: %v", err)
	}
	if _, err := Apply(checkpoint, definition, sentBack("start")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("sending back to the start: %v", err)
	}

	back, err := Apply(checkpoint, definition, sentBack("implement"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(back, definition); err != nil {
		t.Fatalf("classify after send-back: %v", err)
	}
	back = roundTrip(t, back, definition)
	if back.Nodes["implement"] != NodeReady || back.Nodes["review"] != NodePending || back.Nodes["decide"] != NodePending || len(back.AwaitingDecisions) != 0 {
		t.Fatalf("after send-back = %v / %v", back.Nodes, back.AwaitingDecisions)
	}
	if _, err := Apply(back, definition, decided("decide", "approved")); err == nil {
		t.Fatal("a verdict formed before the send-back must be refused")
	}
	back, again := succeedUntilQuiet(t, back, definition)
	if len(again) != 2 || again[0].NodeID != "implement" || again[0].Attempt != 2 {
		t.Fatalf("after send-back ran %v", again)
	}
	done, err := Apply(back, definition, decidedAt("decide", "approved", definition.RequiredDecisionAttempt(back, "decide")))
	if err != nil {
		t.Fatal(err)
	}
	done, _ = succeedUntilQuiet(t, done, definition)
	if done.Status != RunCompleted {
		t.Fatalf("status = %q", done.Status)
	}
}

// Work still out downstream: an agent step is withdrawn, a program refuses
// the send-back.
func TestSendBackWithdrawsAgentWorkAndRefusesARunningProgram(t *testing.T) {
	tmpl := steerTemplate()
	ship := tmpl.Nodes["ship"]
	ship.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "ops", Prompt: "Ship it."}
	tmpl.Nodes["ship"] = ship
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-back2", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, work := advanceAndPlan(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, observed(work, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, shipCmd := advanceAndPlan(t, checkpoint, definition)
	if shipCmd == nil || shipCmd.NodeID != "ship" {
		t.Fatalf("ship = %#v", shipCmd)
	}
	back, err := Apply(checkpoint, definition, sentBack("work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Commands) != 0 || back.Nodes["ship"] != NodePending || back.Nodes["work"] != NodeReady {
		t.Fatalf("after send-back = %v / %v", back.Nodes, back.Commands)
	}
	if err := ClassifyCheckpoint(back, definition); err != nil {
		t.Fatalf("classify: %v", err)
	}

	program := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 2, OnExhausted: "escalate"}), nil)
	cp, err := Initialize("run-back3", program)
	if err != nil {
		t.Fatal(err)
	}
	cp, implement := advanceAndPlan(t, cp, program)
	if cp, err = Apply(cp, program, observed(implement, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	cp, _ = advanceAndPlan(t, cp, program) // review is out
	if _, err := Apply(cp, program, sentBack("implement")); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("a running program downstream: %v", err)
	}
}

// Sending back over an agreement gate drops its votes: the deciders vote
// again in new windows.
func TestSendBackResetsAnAgreementGatesVotes(t *testing.T) {
	definition := mustPrepare(t, agreementTemplate(), nil)
	checkpoint, err := Initialize("run-back4", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = runUntilQuiet(t, checkpoint, definition)
	checkpoint = vote(t, checkpoint, definition, 1, "approved")
	back, err := Apply(checkpoint, definition, sentBack("review"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckpoint(back, definition); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, k := range []int{1, 2, 3} {
		if status := back.Nodes[model.GateVoteID("decide", k)]; status != NodePending {
			t.Fatalf("vote %d = %q after send-back", k, status)
		}
	}
	if len(back.AwaitingDecisions) != 0 || back.Nodes["review"] != NodeReady {
		t.Fatalf("after send-back = %v", back.AwaitingDecisions)
	}
	if got := votesOf(t, definition, back); got[0] != "" {
		t.Fatalf("the earlier vote is dropped; votes = %#v", got)
	}
	back, _ = runUntilQuiet(t, back, definition)
	if len(back.AwaitingDecisions) != 3 {
		t.Fatalf("every decider votes again; awaiting = %#v", back.AwaitingDecisions)
	}
}
