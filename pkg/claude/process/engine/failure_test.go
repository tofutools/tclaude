package engine

import "testing"

// A step that fails with nothing authored to handle it ends the run, and the
// run says which step and why. Its siblings, failing only because the run is
// ending, never replace that.
func TestUnhandledFailureNamesTheStepThatEndedTheRun(t *testing.T) {
	definition := mustPrepare(t, fanOutTemplate("left", "right"), nil)
	checkpoint, err := Initialize("run-failure", definition)
	if err != nil {
		t.Fatal(err)
	}
	current, first := advanceAndPlan(t, checkpoint, definition)
	current, second := advanceAndPlan(t, current, definition)
	if first == nil || second == nil {
		t.Fatal("fixture did not plan both branches")
	}
	failed, err := Apply(current, definition, observedWithError(first, "exit 3: tests failed"))
	if err != nil {
		t.Fatal(err)
	}
	want := StepFailure{NodeID: first.NodeID, Attempt: 1, Reason: "exit 3: tests failed"}
	if failed.Failure == nil || *failed.Failure != want {
		t.Fatalf("failure = %#v, want %#v", failed.Failure, want)
	}
	// The sibling, killed as the run ends, fails too: not the reason.
	drained, err := Apply(failed, definition, observedWithError(second, "program canceled: the run is ending"))
	if err != nil {
		t.Fatal(err)
	}
	if drained.Status != RunFailed || drained.Nodes[second.NodeID] != NodeFailed {
		t.Fatalf("the run did not end failed: %#v", drained)
	}
	if *drained.Failure != want {
		t.Fatalf("a sibling replaced the failure: %#v", drained.Failure)
	}
	if err := ValidateCheckpoint(drained, definition); err != nil {
		t.Fatalf("the load boundary refused the failure: %v", err)
	}
	bad := cloneCheckpoint(drained)
	bad.Failure = &StepFailure{NodeID: "join"}
	if err := ValidateCheckpoint(bad, definition); err == nil {
		t.Fatal("a failure naming a node that did not fail was accepted")
	}
}
