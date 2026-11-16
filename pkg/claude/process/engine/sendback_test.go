package engine

import (
	"errors"
	"slices"
	"strings"
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
	back, again := succeedUntilQuiet(t, back, definition)
	if _, err := Apply(back, definition, decidedAt("decide", "approved", 0)); err == nil {
		t.Fatal("a verdict formed in the window before the send-back must be refused once the decision reopens")
	}
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
	if _, err := Apply(checkpoint, definition, sentBack("review")); !errors.Is(err, ErrInvalidTransition) || !strings.Contains(err.Error(), `send the run back to "implement"`) {
		t.Fatalf("into the middle of a loop body: %v", err)
	}
	back, err := Apply(checkpoint, definition, sentBack("implement"))
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
	if len(back.AwaitingDecisions) != 0 || back.Nodes["implement"] != NodeReady {
		t.Fatalf("after send-back = %v", back.AwaitingDecisions)
	}
	if got := votesOf(t, definition, back); got[0] != "" {
		t.Fatalf("the earlier vote is dropped; votes = %#v", got)
	}
	back, _ = runUntilQuiet(t, back, definition)
	if len(back.AwaitingDecisions) != 3 {
		t.Fatalf("every decider votes again; awaiting = %#v", back.AwaitingDecisions)
	}
	voteID := model.GateVoteID("decide", 2)
	if _, err := Apply(back, definition, decidedAt(voteID, "approved", 0)); err == nil {
		t.Fatal("a vote formed in the window before the send-back must be refused")
	}
}

// A one-shot decision sent back to reopens under a new window, so the verdict
// given before is refused; an agent decider sent back over gets a fresh
// attempt window, and the checkpoint still loads.
func TestSendBackReopensDecisionsUnderNewWindows(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)
	checkpoint, err := Initialize("run-back5", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = runUntilQuiet(t, checkpoint, definition)
	obligation := checkpoint.FirstAwaitingDecision()
	if obligation == nil {
		t.Fatal("setup: no decision awaited")
	}
	verdict := definition.nodes[definition.index[obligation.NodeID]].verdicts[0]
	decidedOnce, err := Apply(checkpoint, definition, decided(obligation.NodeID, verdict))
	if err != nil {
		t.Fatal(err)
	}
	back, err := Apply(decidedOnce, definition, sentBack(obligation.NodeID))
	if err != nil {
		t.Fatal(err)
	}
	back = roundTrip(t, back, definition)
	if _, err := Apply(back, definition, decided(obligation.NodeID, verdict)); err == nil {
		t.Fatal("the earlier verdict must not apply to the reopened decision")
	}
	if _, err := Apply(back, definition, decidedAt(obligation.NodeID, verdict, definition.RequiredDecisionAttempt(back, obligation.NodeID))); err != nil {
		t.Fatalf("a verdict in the new window: %v", err)
	}

	tmpl := decisionDiamondTemplate()
	choose := tmpl.Nodes["choose"]
	choose.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Approve?"}
	tmpl.Nodes["choose"] = choose
	agent := mustPrepare(t, tmpl, nil)
	cp, err := Initialize("run-back6", agent)
	if err != nil {
		t.Fatal(err)
	}
	cp, intake := advanceAndPlan(t, cp, agent)
	if cp, err = Apply(cp, agent, observed(intake, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	cp, decider := advanceAndPlan(t, cp, agent)
	if decider == nil || decider.NodeID != "choose" {
		t.Fatalf("decider = %#v", decider)
	}
	if cp, err = Apply(cp, agent, reportedVerdict(decider, "approve")); err != nil {
		t.Fatal(err)
	}
	cp, fast := advanceAndPlan(t, cp, agent)
	if cp, err = Apply(cp, agent, observed(fast, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	back, err = Apply(cp, agent, sentBack("intake"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckpoint(back, agent); err != nil {
		t.Fatalf("a send-back over an agent decider must still load: %v", err)
	}
	back, intake = advanceAndPlan(t, back, agent)
	if back, err = Apply(back, agent, observed(intake, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	_, again := advanceAndPlan(t, back, agent)
	if again == nil || again.NodeID != "choose" || again.Attempt != 2 {
		t.Fatalf("the agent decider decides again as attempt 2: %#v", again)
	}
}
