package engine

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// agreementTemplate is the review loop with an agreement gate for its
// decision: three people vote, approved needs all three.
//
//	start -> implement -> review -> decide -{approved}-> merge -> end
//	                                       -{changes}-> implement (2 rounds)
//	                                       -{escalate}-> escalated
func agreementTemplate() *model.Template {
	tmpl := reviewLoopTemplate(model.LoopBound{MaxIterations: 2, OnExhausted: "escalate"})
	decide := tmpl.Nodes["decide"]
	decide.Performer = nil
	decide.Agree = "approved"
	for _, ask := range []string{"Ana: ship it?", "Bo: ship it?", "Cy: ship it?"} {
		decide.Deciders = append(decide.Deciders, model.Performer{Kind: model.PerformerHuman, Ask: ask})
	}
	tmpl.Nodes["decide"] = decide
	return tmpl
}

// vote records one decider's verdict in its current window and checks the
// state stays valid.
func vote(t *testing.T, checkpoint Checkpoint, definition *Definition, k int, verdict string) Checkpoint {
	t.Helper()
	voteID := model.GateVoteID("decide", k)
	next, err := Apply(checkpoint, definition,
		decidedAt(voteID, verdict, definition.RequiredDecisionAttempt(checkpoint, voteID)))
	if err != nil {
		t.Fatalf("vote %d %s: %v", k, verdict, err)
	}
	next, err = AdvanceUntilQuiescent(next, definition)
	if err != nil {
		t.Fatalf("advance after vote %d: %v", k, err)
	}
	// Votes run alongside each other, so the sequential classifier does not
	// apply (ClassifyCheckpoint); the structural validator does.
	if err := ValidateCheckpoint(next, definition); err != nil {
		t.Fatalf("validate after vote %d: %v", k, err)
	}
	return roundTrip(t, next, definition)
}

func votesOf(t *testing.T, definition *Definition, checkpoint Checkpoint) []string {
	t.Helper()
	gates := definition.Gates(checkpoint)
	if len(gates) != 1 || gates[0].NodeID != "decide" || gates[0].Agree != "approved" {
		t.Fatalf("gates = %#v", gates)
	}
	var verdicts []string
	for _, v := range gates[0].Votes {
		verdicts = append(verdicts, v.Verdict)
	}
	return verdicts
}

func TestAgreementGate_UnanimousTakesAgree(t *testing.T) {
	definition := mustPrepare(t, agreementTemplate(), nil)
	checkpoint, err := Initialize("run-agree", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = runUntilQuiet(t, checkpoint, definition)
	if len(checkpoint.AwaitingDecisions) != 3 {
		t.Fatalf("every decider votes at once; awaiting = %#v", checkpoint.AwaitingDecisions)
	}
	checkpoint = vote(t, checkpoint, definition, 2, "approved")
	checkpoint = vote(t, checkpoint, definition, 1, "approved")
	if got := votesOf(t, definition, checkpoint); got[0] != "approved" || got[1] != "approved" || got[2] != "" {
		t.Fatalf("tally while waiting on Cy = %#v", got)
	}
	if status := checkpoint.Nodes["merge"]; status != NodePending {
		t.Fatalf("merge = %q with a vote still out", status)
	}
	before := checkpoint
	checkpoint = vote(t, checkpoint, definition, 3, "approved")
	settled := definition.GateSettlements(before, checkpoint)
	if len(settled) != 1 || settled[0].GateID != "decide" || settled[0].Verdict != "approved" {
		t.Fatalf("settlements = %#v", settled)
	}
	checkpoint, ran := runUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "merge" || checkpoint.Status != RunCompleted {
		t.Fatalf("after agreement ran %#v, status %q", ran, checkpoint.Status)
	}
}

func TestAgreementGate_FirstDissentRoutesAndLoopsThenExhausts(t *testing.T) {
	definition := mustPrepare(t, agreementTemplate(), nil)
	checkpoint, err := Initialize("run-dissent", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = runUntilQuiet(t, checkpoint, definition)
	// Cy escalates before Bo asks for changes, but Bo comes first in decider
	// order: the gate takes changes.
	checkpoint = vote(t, checkpoint, definition, 3, "escalate")
	checkpoint = vote(t, checkpoint, definition, 1, "approved")
	before := checkpoint
	checkpoint = vote(t, checkpoint, definition, 2, "changes")
	if settled := definition.GateSettlements(before, checkpoint); len(settled) != 1 || settled[0].Verdict != "changes" {
		t.Fatalf("settlements = %#v", settled)
	}
	if got := checkpoint.Iterations[model.GateTallyID("decide")]["changes"]; got != 1 {
		t.Fatalf("iterations = %#v", checkpoint.Iterations)
	}
	if got := votesOf(t, definition, checkpoint); got[0] != "" || got[1] != "" || got[2] != "" {
		t.Fatalf("a new round starts with no votes; got %#v", got)
	}
	checkpoint, ran := runUntilQuiet(t, checkpoint, definition)
	if len(ran) != 2 || ran[0].NodeID != "implement" || ran[0].Round != 2 ||
		ran[0].LoopFrom != model.GateTallyID("decide") {
		t.Fatalf("round 2 = %#v", ran)
	}
	if len(checkpoint.AwaitingDecisions) != 3 {
		t.Fatalf("round 2 asks every decider again; awaiting = %#v", checkpoint.AwaitingDecisions)
	}
	// Round 2 is the last: another change request follows onExhausted.
	checkpoint = vote(t, checkpoint, definition, 1, "changes")
	checkpoint = vote(t, checkpoint, definition, 2, "approved")
	checkpoint = vote(t, checkpoint, definition, 3, "approved")
	if checkpoint.Status != RunCanceled || checkpoint.Nodes["merge"] != NodeSkipped {
		t.Fatalf("status = %q, merge = %q", checkpoint.Status, checkpoint.Nodes["merge"])
	}
}

func TestAgreementGate_Validation(t *testing.T) {
	codes := func(edit func(*model.Node)) []string {
		tmpl := agreementTemplate()
		node := tmpl.Nodes["decide"]
		edit(&node)
		tmpl.Nodes["decide"] = node
		var out []string
		for _, d := range model.Validate(tmpl, model.NormalizeEdges(tmpl)) {
			if d.Severity == model.SeverityError {
				out = append(out, d.Code)
			}
		}
		return out
	}
	expect := func(name string, edit func(*model.Node), code string) {
		t.Helper()
		got := codes(edit)
		for _, c := range got {
			if c == code {
				return
			}
		}
		t.Errorf("%s: codes = %v, want %s", name, got, code)
	}
	if got := codes(func(*model.Node) {}); len(got) != 0 {
		t.Fatalf("the fixture is valid; got %v", got)
	}
	expect("no agree", func(n *model.Node) { n.Agree = "" }, "missing_agree")
	expect("unrouted agree", func(n *model.Node) { n.Agree = "ship" }, "agree_outcome")
	expect("one decider", func(n *model.Node) { n.Deciders = n.Deciders[:1] }, "gate_deciders")
	expect("performer too", func(n *model.Node) {
		n.Performer = &model.Performer{Kind: model.PerformerHuman, Ask: "?"}
	}, "gate_performer")
	expect("program decider", func(n *model.Node) {
		n.Deciders[0] = model.Performer{Kind: model.PerformerProgram, Run: "true"}
	}, "gate_decider_kind")
	expect("loop with no way out", func(n *model.Node) {
		n.Loops = map[string]model.LoopBound{"changes": {MaxIterations: 2}}
	}, "gate_loop_exhausted")
	expect("agree on a plain decision", func(n *model.Node) { n.Deciders = nil }, "agree_without_deciders")

	whole := func(name, code string, edit func(*model.Template)) {
		t.Helper()
		tmpl := agreementTemplate()
		edit(tmpl)
		for _, d := range model.Validate(tmpl, model.NormalizeEdges(tmpl)) {
			if d.Code == code {
				return
			}
		}
		t.Errorf("%s: want %s", name, code)
	}
	whole("deciders on a task", "gate_on_non_decision", func(tmpl *model.Template) {
		task := tmpl.Nodes["merge"]
		task.Agree = "approved"
		tmpl.Nodes["merge"] = task
	})
	whole("sameAs a gate", "invalid_performer_constraint", func(tmpl *model.Template) {
		gate := tmpl.Nodes["decide"]
		gate.Deciders[0] = model.Performer{Kind: model.PerformerAgent, Prompt: "Review."}
		tmpl.Nodes["decide"] = gate
		merge := tmpl.Nodes["merge"]
		merge.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Merge.", SameAs: "decide"}
		tmpl.Nodes["merge"] = merge
	})

	tmpl := agreementTemplate()
	tmpl.Nodes["decide.vote.1"] = model.Node{Type: model.NodeTypeEnd}
	found := false
	for _, d := range model.Validate(tmpl, model.NormalizeEdges(tmpl)) {
		found = found || d.Code == "node_id_collides_with_expansion"
	}
	if !found {
		t.Error("an authored node named like a vote collides with the gate's expansion")
	}
}

// runUntilQuiet is succeedUntilQuiet for a run with votes in flight.
func runUntilQuiet(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, []Command) {
	t.Helper()
	var ran []Command
	for {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if err := ValidateCheckpoint(checkpoint, definition); err != nil {
			t.Fatalf("validate after plan: %v", err)
		}
		if command == nil {
			return checkpoint, ran
		}
		ran = append(ran, *command)
		var err error
		if checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0)); err != nil {
			t.Fatal(err)
		}
	}
}
