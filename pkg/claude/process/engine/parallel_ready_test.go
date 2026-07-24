package engine

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestEngineGeneratedStatesPassStrictClassifierAndStayBounded drives a whole
// decision-diamond run and asserts the strict diagnostic classifier accepts
// every engine-generated state and that the parallel-ready outbox arrays never
// exceed one entry in the current TCL-650 slice. The classifier is checked
// after every engine-owned and manual transition, so the slice keeps its full
// whole-graph bug-finding coverage even though the runtime hot path no longer
// runs it.
func TestEngineGeneratedStatesPassStrictClassifierAndStayBounded(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)

	current, err := Initialize("run-strict", definition)
	if err != nil {
		t.Fatal(err)
	}
	current = stepAssertingClassifier(t, current, definition)
	current = applyAssertingClassifier(t, current, definition, observed(current.OutstandingCommand(), ProgramSucceeded, 0))
	current = stepAssertingClassifier(t, current, definition)
	if current.AwaitingDecision() == nil {
		t.Fatalf("run did not park on its decision: %#v", current)
	}
	current = applyAssertingClassifier(t, current, definition, decided("choose", "approve"))
	current = stepAssertingClassifier(t, current, definition)
	current = applyAssertingClassifier(t, current, definition, observed(current.OutstandingCommand(), ProgramSucceeded, 0))
	current = stepAssertingClassifier(t, current, definition)
	current = applyAssertingClassifier(t, current, definition, observed(current.OutstandingCommand(), ProgramSucceeded, 0))
	current = stepAssertingClassifier(t, current, definition)
	if current.Status != RunCompleted {
		t.Fatalf("run did not complete: %#v", current)
	}
}

// stepAssertingClassifier applies every engine-owned transition until quiescent,
// asserting the strict classifier and the single-entry outbox bound before each.
func stepAssertingClassifier(t *testing.T, checkpoint Checkpoint, definition *Definition) Checkpoint {
	t.Helper()
	for {
		assertClassifiedAndBounded(t, checkpoint, definition)
		transition, ok := nextEngineTransition(checkpoint, definition)
		if !ok {
			return checkpoint
		}
		next, err := apply(checkpoint, definition, transition)
		if err != nil {
			t.Fatalf("engine transition %q: %v", transition.Kind, err)
		}
		checkpoint = next
	}
}

func applyAssertingClassifier(t *testing.T, checkpoint Checkpoint, definition *Definition, transition Transition) Checkpoint {
	t.Helper()
	next, err := Apply(checkpoint, definition, transition)
	if err != nil {
		t.Fatalf("manual transition %q: %v", transition.Kind, err)
	}
	assertClassifiedAndBounded(t, next, definition)
	return next
}

func assertClassifiedAndBounded(t *testing.T, checkpoint Checkpoint, definition *Definition) {
	t.Helper()
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("engine-generated state failed the strict classifier: %v\nstate: %#v", err, checkpoint)
	}
	if len(checkpoint.Commands) > 1 {
		t.Fatalf("decision-only slice produced %d commands: %#v", len(checkpoint.Commands), checkpoint.Commands)
	}
	if len(checkpoint.AwaitingDecisions) > 1 {
		t.Fatalf("decision-only slice produced %d obligations: %#v", len(checkpoint.AwaitingDecisions), checkpoint.AwaitingDecisions)
	}
}

// TestRuntimeBoundaryLoadsStructurallySafePluralState proves the load/persist
// boundary accepts structurally safe parallel-ready state — here two running
// program commands — even though the current-slice single-active proof rejects
// it. Only the strict diagnostic classifier enforces that slice expectation.
func TestRuntimeBoundaryLoadsStructurallySafePluralState(t *testing.T) {
	definition := mustPrepare(t, sequentialTemplate("first", "second"), nil)
	base, err := Initialize("run-plural", definition)
	if err != nil {
		t.Fatal(err)
	}

	plural := cloneCheckpoint(base)
	plural.Nodes["first"] = NodeRunning
	plural.Nodes["second"] = NodeRunning
	plural.Commands = []Command{
		programCommand(plural.RunID, definition.nodes[definition.index["first"]]),
		programCommand(plural.RunID, definition.nodes[definition.index["second"]]),
	}

	if err := ValidateCheckpoint(plural, definition); err != nil {
		t.Fatalf("runtime validator must accept structurally safe plural state: %v", err)
	}
	encoded, err := json.Marshal(plural)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(encoded, definition); err != nil {
		t.Fatalf("persistence boundary must load structurally safe plural state: %v", err)
	}
	if err := ClassifyCheckpoint(plural, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("strict classifier must reject the two-active slice violation: %v", err)
	}
}

// TestStructuralBoundaryRejectsMalformedCommandsAndObligations covers the
// concrete structural safety the runtime load/persist validator retains for the
// parallel-ready outbox arrays: unknown, mistyped, duplicate, non-deterministic,
// and wrong-node-status command/obligation entries fail closed regardless of how
// many entries the current reducer produces.
func TestStructuralBoundaryRejectsMalformedCommandsAndObligations(t *testing.T) {
	commandDefinition := mustPrepare(t, sequentialTemplate("task"), nil)
	runningCommand, err := Initialize("run-shape", commandDefinition)
	if err != nil {
		t.Fatal(err)
	}
	if runningCommand, err = AdvanceUntilQuiescent(runningCommand, commandDefinition); err != nil {
		t.Fatal(err)
	}
	if runningCommand.OutstandingCommand() == nil {
		t.Fatalf("fixture did not reach a running command: %#v", runningCommand)
	}

	obligationDefinition := mustPrepare(t, decisionDiamondTemplate(), nil)
	awaitingDecision := advanceToDecision(t, "run-shape-decision", obligationDefinition, "choose", "intake")

	tests := []struct {
		name       string
		definition *Definition
		base       Checkpoint
		corrupt    func(*Checkpoint)
	}{
		{"command kind", commandDefinition, runningCommand, func(c *Checkpoint) { c.Commands[0].Kind = "bogus" }},
		{"command unknown node", commandDefinition, runningCommand, func(c *Checkpoint) { c.Commands[0].NodeID = "ghost" }},
		{"command non-task node", commandDefinition, runningCommand, func(c *Checkpoint) { c.Commands[0].NodeID = "end" }},
		{"command non-deterministic id", commandDefinition, runningCommand, func(c *Checkpoint) { c.Commands[0].ID = "forged" }},
		{"duplicate command", commandDefinition, runningCommand, func(c *Checkpoint) { c.Commands = append(c.Commands, c.Commands[0]) }},
		{"command node not running", commandDefinition, runningCommand, func(c *Checkpoint) { c.Nodes["task"] = NodeReady }},
		{"obligation unknown node", obligationDefinition, awaitingDecision, func(c *Checkpoint) { c.AwaitingDecisions[0].NodeID = "ghost" }},
		{"obligation non-decision node", obligationDefinition, awaitingDecision, func(c *Checkpoint) { c.AwaitingDecisions[0].NodeID = "merge" }},
		{"duplicate obligation", obligationDefinition, awaitingDecision, func(c *Checkpoint) { c.AwaitingDecisions = append(c.AwaitingDecisions, c.AwaitingDecisions[0]) }},
		{"obligation node not ready", obligationDefinition, awaitingDecision, func(c *Checkpoint) { c.Nodes["choose"] = NodePending }},
		{"obligation on terminal run", obligationDefinition, awaitingDecision, func(c *Checkpoint) { c.Status = RunCompleted }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broken := cloneCheckpoint(test.base)
			test.corrupt(&broken)
			if err := ValidateCheckpoint(broken, test.definition); !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("structural validator error = %v", err)
			}
		})
	}
}

// TestReducerMonotonicGuardsRejectResettlementAndRegression proves the local
// reducer guards that replace the removed whole-graph exit validation: a settled
// edge never re-settles, and a node that already reached a final state never
// completes again.
func TestReducerMonotonicGuardsRejectResettlementAndRegression(t *testing.T) {
	definition := mustPrepare(t, sequentialTemplate("task"), nil)
	base, err := Initialize("run-mono", definition)
	if err != nil {
		t.Fatal(err)
	}

	// Final-node regression: completing an already-done node fails closed.
	regressed := cloneCheckpoint(base)
	regressed.Nodes["start"] = NodeDone
	if err := completeAndSettle(&regressed, definition, definition.index["start"], defaultEdgeOutcome(definition, "start")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("final-node regression guard error = %v", err)
	}

	// Edge re-settlement: an active node whose outgoing edge is already settled
	// cannot settle it a second time.
	resettled := cloneCheckpoint(base)
	resettled.Edges["start"][defaultEdgeOutcome(definition, "start")] = EdgeArrived
	if _, err := Apply(resettled, definition, Transition{Kind: TransitionAdvance}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("edge re-settlement guard error = %v", err)
	}
}

func defaultEdgeOutcome(definition *Definition, nodeID string) string {
	node := definition.nodes[definition.index[nodeID]]
	return definition.edges[node.outgoing[0]].outcome
}

// TestRuntimeCycleSkipsWholeCheckpointValidation proves the per-transition
// runtime cycle does NOT run the O(nodes+edges) whole-checkpoint validator:
// given state the trusted boundary validator (ValidateCheckpoint) rejects but
// whose local transition preconditions still hold, Plan/Apply/AdvanceUntilQuiescent
// proceed instead of re-scanning and failing. The whole-checkpoint scan is
// confined to the load (DecodeCheckpoint) and creation (Initialize) boundaries.
func TestRuntimeCycleSkipsWholeCheckpointValidation(t *testing.T) {
	definition := mustPrepare(t, sequentialTemplate("task"), nil)
	valid, err := Initialize("run-novalidate", definition)
	if err != nil {
		t.Fatal(err)
	}

	// Taint the checkpoint with an extra unknown node so the boundary validator
	// rejects it, without disturbing the start-advance transition's local
	// preconditions (the real active node is still the sole ready start node).
	tainted := cloneCheckpoint(valid)
	tainted.Nodes["ghost"] = NodePending
	if err := ValidateCheckpoint(tainted, definition); err == nil {
		t.Fatal("boundary validator must reject the tainted checkpoint")
	}

	if _, err := Plan(tainted, definition); err != nil {
		t.Fatalf("Plan must not re-run whole-checkpoint validation: %v", err)
	}
	advanced, err := Apply(tainted, definition, Transition{Kind: TransitionAdvance})
	if err != nil {
		t.Fatalf("Apply must not re-run whole-checkpoint validation: %v", err)
	}
	if advanced.Nodes["start"] != NodeDone || advanced.Nodes["task"] != NodeReady {
		t.Fatalf("advance did not progress on the tainted state: %#v", advanced.Nodes)
	}
	if _, ok := advanced.Nodes["ghost"]; !ok {
		t.Fatal("reducer unexpectedly scanned/pruned the unrelated node")
	}
	if _, err := AdvanceUntilQuiescent(tainted, definition); err != nil {
		t.Fatalf("AdvanceUntilQuiescent must not re-run whole-checkpoint validation: %v", err)
	}
}
