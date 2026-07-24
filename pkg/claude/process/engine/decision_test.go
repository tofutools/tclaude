package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// decisionDiamondTemplate is the canonical exclusive diamond: a decision
// branches to two program tasks that converge on one local merge task.
//
//	start -> intake -> choose -{approve}-> fast  \
//	                          -{reject} -> slow  -> merge -> end
func decisionDiamondTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "decision-diamond",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "intake"}},
			"intake": programTask("choose", "intake"),
			"choose": humanDecision("Approve?", model.Next{"approve": "fast", "reject": "slow"}),
			"fast":   programTask("merge", "fast"),
			"slow":   programTask("merge", "slow"),
			"merge":  programTask("end", "merge"),
			"end":    {Type: model.NodeTypeEnd},
		},
	}
}

// decisionDirectMergeTemplate routes one decision outcome straight into the
// merge node while the other passes through an intermediate task first.
func decisionDirectMergeTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "decision-direct-merge",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "choose"}},
			"choose": humanDecision("Detour?", model.Next{"detour": "extra", "skip": "merge"}),
			"extra":  programTask("merge", "extra"),
			"merge":  programTask("end", "merge"),
			"end":    {Type: model.NodeTypeEnd},
		},
	}
}

// decisionMultiEndTemplate lets a verdict select between end nodes with
// different terminal results, leaving a whole branch impossible.
func decisionMultiEndTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "decision-multi-end",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "choose"}},
			"choose":   humanDecision("Proceed?", model.Next{"proceed": "work", "abort": "canceled"}),
			"work":     programTask("done", "work"),
			"done":     {Type: model.NodeTypeEnd},
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
}

func humanDecision(ask string, next model.Next) model.Node {
	return model.Node{
		Type:      model.NodeTypeDecision,
		Performer: &model.Performer{Kind: model.PerformerHuman, Ask: ask},
		Next:      next,
	}
}

func decided(nodeID, verdict string) Transition {
	return Transition{Kind: TransitionDecisionRecorded, Decision: &DecisionRecord{NodeID: nodeID, Verdict: verdict}}
}

// advanceToDecision drives a fresh run through start (and any tasks named in
// succeed) until the named decision is the awaited obligation.
func advanceToDecision(t *testing.T, runID string, definition *Definition, decisionID string, succeed ...string) Checkpoint {
	t.Helper()
	checkpoint, err := Initialize(runID, definition)
	if err != nil {
		t.Fatal(err)
	}
	for {
		checkpoint, err = AdvanceUntilQuiescent(checkpoint, definition)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.OutstandingCommand() == nil {
			break
		}
		nodeID := checkpoint.OutstandingCommand().NodeID
		allowed := false
		for _, taskID := range succeed {
			allowed = allowed || taskID == nodeID
		}
		if !allowed {
			t.Fatalf("unexpected command for node %q on the way to decision %q", nodeID, decisionID)
		}
		checkpoint, err = Apply(checkpoint, definition, observed(checkpoint.OutstandingCommand(), ProgramSucceeded, 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	if checkpoint.AwaitingDecision() == nil || checkpoint.AwaitingDecision().NodeID != decisionID {
		t.Fatalf("awaiting decision = %#v, want node %q", checkpoint.AwaitingDecision(), decisionID)
	}
	return checkpoint
}

func TestDecisionVerdictClosesAlternativesAndActivatesChosenBranch(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)
	checkpoint := advanceToDecision(t, "run-diamond", definition, "choose", "intake")

	if command, err := Plan(checkpoint, definition); err != nil || command != nil {
		t.Fatalf("awaiting-decision plan = %#v, %v", command, err)
	}
	if verdicts, ok := definition.DecisionVerdicts("choose"); !ok || !reflect.DeepEqual(verdicts, []string{"approve", "reject"}) {
		t.Fatalf("decision verdicts = %#v, %t", verdicts, ok)
	}

	next, err := Apply(checkpoint, definition, decided("choose", "approve"))
	if err != nil {
		t.Fatal(err)
	}
	if next.AwaitingDecision() != nil || next.Nodes["choose"] != NodeDone {
		t.Fatalf("post-decision checkpoint = %#v", next)
	}
	if next.Edges["choose"]["approve"] != EdgeArrived || next.Edges["choose"]["reject"] != EdgeNotTaken {
		t.Fatalf("decision edges = %#v", next.Edges["choose"])
	}
	if next.Nodes["fast"] != NodeReady {
		t.Fatalf("chosen branch node = %q", next.Nodes["fast"])
	}
	// The rejected branch is impossible: its task is skipped and its own
	// outgoing edge into the merge closes transitively.
	if next.Nodes["slow"] != NodeSkipped || next.Edges["slow"]["next"] != EdgeNotTaken {
		t.Fatalf("closed branch = node %q edge %q", next.Nodes["slow"], next.Edges["slow"]["next"])
	}
	// The merge stays pending until its remaining live candidate arrives.
	if next.Nodes["merge"] != NodePending {
		t.Fatalf("merge = %q", next.Nodes["merge"])
	}

	running, err := AdvanceUntilQuiescent(next, definition)
	if err != nil {
		t.Fatal(err)
	}
	afterFast, err := Apply(running, definition, observed(running.OutstandingCommand(), ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if afterFast.Nodes["merge"] != NodeReady {
		t.Fatalf("merge after chosen-branch arrival = %q", afterFast.Nodes["merge"])
	}
	running, err = AdvanceUntilQuiescent(afterFast, definition)
	if err != nil {
		t.Fatal(err)
	}
	afterMerge, err := Apply(running, definition, observed(running.OutstandingCommand(), ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := AdvanceUntilQuiescent(afterMerge, definition)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RunCompleted {
		t.Fatalf("status = %q", completed.Status)
	}
	want := map[string]NodeStatus{
		"start": NodeDone, "intake": NodeDone, "choose": NodeDone,
		"fast": NodeDone, "slow": NodeSkipped, "merge": NodeDone, "end": NodeDone,
	}
	if !reflect.DeepEqual(completed.Nodes, want) {
		t.Fatalf("terminal nodes = %#v", completed.Nodes)
	}
}

func TestDecisionDirectMergeEdgeActivatesOnlyWhenCandidateSetSettles(t *testing.T) {
	definition := mustPrepare(t, decisionDirectMergeTemplate(), nil)
	checkpoint := advanceToDecision(t, "run-direct", definition, "choose")

	next, err := Apply(checkpoint, definition, decided("choose", "skip"))
	if err != nil {
		t.Fatal(err)
	}
	// Choosing the direct edge closes the detour branch transitively; only
	// then is the merge's candidate set settled and the merge activates.
	if next.Nodes["extra"] != NodeSkipped || next.Edges["extra"]["next"] != EdgeNotTaken {
		t.Fatalf("detour branch = node %q edge %q", next.Nodes["extra"], next.Edges["extra"]["next"])
	}
	if next.Nodes["merge"] != NodeReady {
		t.Fatalf("merge = %q", next.Nodes["merge"])
	}

	detour, err := Apply(checkpoint, definition, decided("choose", "detour"))
	if err != nil {
		t.Fatal(err)
	}
	// The direct edge is closed but the detour branch is still live, so the
	// merge must keep waiting for the detour task.
	if detour.Nodes["merge"] != NodePending || detour.Nodes["extra"] != NodeReady {
		t.Fatalf("detour checkpoint: merge %q extra %q", detour.Nodes["merge"], detour.Nodes["extra"])
	}
	if detour.Edges["choose"]["skip"] != EdgeNotTaken || detour.Edges["extra"]["next"] != EdgeUnresolved {
		t.Fatalf("detour edges = %#v", detour.Edges)
	}
}

func TestDecisionSelectsTerminalEndAndClosesImpossiblePath(t *testing.T) {
	definition := mustPrepare(t, decisionMultiEndTemplate(), nil)
	checkpoint := advanceToDecision(t, "run-abort", definition, "choose")

	next, err := Apply(checkpoint, definition, decided("choose", "abort"))
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := AdvanceUntilQuiescent(next, definition)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != RunCanceled {
		t.Fatalf("status = %q", canceled.Status)
	}
	want := map[string]NodeStatus{
		"start": NodeDone, "choose": NodeDone,
		"work": NodeSkipped, "done": NodeSkipped, "canceled": NodeDone,
	}
	if !reflect.DeepEqual(canceled.Nodes, want) {
		t.Fatalf("terminal nodes = %#v", canceled.Nodes)
	}
	if command, err := Plan(canceled, definition); err != nil || command != nil {
		t.Fatalf("terminal plan = %#v, %v", command, err)
	}
}

func TestStaleDuplicateWrongNodeAndInvalidVerdictDecisionsAreRefused(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)

	initial, err := Initialize("run-refuse", definition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(initial, definition, decided("choose", "approve")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("unsolicited decision error = %v", err)
	}

	checkpoint := advanceToDecision(t, "run-refuse", definition, "choose", "intake")
	if _, err := Apply(checkpoint, definition, decided("merge", "approve")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("wrong-node error = %v", err)
	}
	if _, err := Apply(checkpoint, definition, decided("choose", "maybe")); !errors.Is(err, ErrInvalidDecisionVerdict) {
		t.Fatalf("invalid-verdict error = %v", err)
	}
	before := cloneCheckpoint(checkpoint)

	next, err := Apply(checkpoint, definition, decided("choose", "reject"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checkpoint, before) {
		t.Fatalf("reducer mutated its input checkpoint")
	}
	if _, err := Apply(next, definition, decided("choose", "reject")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := Apply(next, definition, decided("choose", "approve")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("late conflicting error = %v", err)
	}
	if next.Nodes["slow"] != NodeReady || next.Nodes["fast"] != NodeSkipped {
		t.Fatalf("post-reject checkpoint = %#v", next.Nodes)
	}

	malformed := decided("choose", "approve")
	malformed.Command = &Command{}
	if _, err := Apply(checkpoint, definition, malformed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("mixed-payload error = %v", err)
	}
}

func TestAwaitingDecisionCheckpointSurvivesRestartRoundTrip(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)
	checkpoint := advanceToDecision(t, "run-restart", definition, "choose", "intake")

	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded, checkpoint) {
		t.Fatalf("restart round trip drifted\n got: %#v\nwant: %#v", reloaded, checkpoint)
	}
	next, err := Apply(reloaded, definition, decided("choose", "approve"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Nodes["fast"] != NodeReady {
		t.Fatalf("post-restart decision did not activate the chosen branch: %#v", next.Nodes)
	}
}

// TestClassifierRejectsBrokenDecisionAndEdgeInvariants exercises the strict
// diagnostic classifier, not the runtime load/persist validator. The
// whole-graph exclusive-decision invariants (single active node, arrival
// exclusivity, activation proofs) are current-slice expectations demoted out of
// the runtime hot path, so they are asserted here via ClassifyCheckpoint.
func TestClassifierRejectsBrokenDecisionAndEdgeInvariants(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)
	base := advanceToDecision(t, "run-invariants", definition, "choose", "intake")

	tests := []struct {
		name    string
		corrupt func(checkpoint *Checkpoint)
	}{
		{"missing edges map", func(c *Checkpoint) { c.Edges = nil }},
		{"missing edge outcome", func(c *Checkpoint) { delete(c.Edges["choose"], "approve") }},
		{"unknown edge disposition", func(c *Checkpoint) { c.Edges["choose"]["approve"] = "perhaps" }},
		{"missing awaiting obligation", func(c *Checkpoint) { c.AwaitingDecisions = nil }},
		{"awaiting non-decision node", func(c *Checkpoint) { c.AwaitingDecisions = []DecisionObligation{{NodeID: "merge"}} }},
		{"two active nodes", func(c *Checkpoint) { c.Nodes["merge"] = NodeReady }},
		{"double arrival", func(c *Checkpoint) {
			c.Nodes["choose"] = NodeDone
			c.AwaitingDecisions = nil
			c.Edges["choose"]["approve"] = EdgeArrived
			c.Edges["choose"]["reject"] = EdgeNotTaken
			c.Nodes["fast"] = NodeDone
			c.Nodes["slow"] = NodeSkipped
			c.Edges["fast"]["next"] = EdgeArrived
			c.Edges["slow"]["next"] = EdgeArrived
			c.Nodes["merge"] = NodeReady
		}},
		{"skipped node with live incoming", func(c *Checkpoint) { c.Nodes["slow"] = NodeSkipped }},
		{"arrival parked on a pending merge", func(c *Checkpoint) {
			c.Nodes["choose"] = NodeDone
			c.AwaitingDecisions = nil
			c.Edges["choose"]["approve"] = EdgeArrived
			c.Edges["choose"]["reject"] = EdgeNotTaken
			c.Nodes["fast"] = NodeDone
			c.Nodes["slow"] = NodeSkipped
			c.Edges["fast"]["next"] = EdgeArrived
			c.Edges["slow"]["next"] = EdgeNotTaken
			// The merge's candidate set is settled with one arrival, yet the
			// state parks it pending instead of activating it.
			c.Nodes["merge"] = NodePending
		}},
		{"pending node with settled candidates", func(c *Checkpoint) {
			c.Nodes["choose"] = NodePending
			c.AwaitingDecisions = nil
		}},
		{"terminal with unsettled branch", func(c *Checkpoint) {
			c.Status = RunCompleted
			c.Nodes["choose"] = NodeDone
			c.AwaitingDecisions = nil
			c.Edges["choose"]["approve"] = EdgeArrived
			c.Edges["choose"]["reject"] = EdgeNotTaken
			c.Nodes["end"] = NodeDone
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broken := cloneCheckpoint(base)
			test.corrupt(&broken)
			if err := ClassifyCheckpoint(broken, definition); !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("classifier error = %v", err)
			}
		})
	}
}
