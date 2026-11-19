package engine

import (
	"encoding/json"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// waitTemplate is start → hold (a wait) → after → end.
func waitTemplate(wait model.WaitConfig) *model.Template {
	tmpl := sequentialTemplate("hold", "after")
	tmpl.Params = map[string]model.Param{"at": {Type: "string"}}
	tmpl.Nodes["hold"] = model.Node{Type: model.NodeTypeWait, Wait: &wait, Next: model.Next{model.DefaultOutcome: "after"}}
	return tmpl
}

// A wait step plans a wait command carrying its params-bound spec, and
// settles along its route when it is observed: elapsed or skipped succeed.
func TestWaitStepPlansATimerAndSettlesAlongItsRoute(t *testing.T) {
	tmpl := waitTemplate(model.WaitConfig{Duration: "3 days", Until: "{{ params.at }}"})
	if diagnostics := CheckEligibility(tmpl); len(diagnostics) > 0 {
		t.Fatalf("a duration/until wait is executable, got %#v", diagnostics)
	}
	definition := mustPrepare(t, tmpl, map[string]string{"at": "2026-12-01T09:00:00Z"})
	checkpoint, err := Initialize("run-wait", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.Kind != CommandWait || command.NodeID != "hold" || command.Wait == nil {
		t.Fatalf("command = %#v", command)
	}
	if *command.Wait != (WaitCommand{Duration: "3 days", Until: "2026-12-01T09:00:00Z"}) {
		t.Fatalf("bound wait = %#v", *command.Wait)
	}
	if !command.Kind.External() {
		t.Fatal("a wait is accounted outside the owner")
	}
	// The planned command survives a cold load: the checkpoint validates.
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(encoded, definition); err != nil {
		t.Fatalf("a checkpoint with a wait outstanding must load: %v", err)
	}
	settled, err := Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if settled.Nodes["hold"] != NodeDone || len(settled.Commands) != 0 {
		t.Fatalf("settled = %#v", settled)
	}
	_, next := advanceAndPlan(t, settled, definition)
	if next == nil || next.NodeID != "after" {
		t.Fatalf("after the wait the run goes on to after, got %#v", next)
	}
}

// A wait that fails (its deadline could not be worked out, or it was
// withdrawn) fails the run naming the step: a wait has no retries.
func TestFailedWaitFailsTheRunNamingTheStep(t *testing.T) {
	definition := mustPrepare(t, waitTemplate(model.WaitConfig{Until: "{{ outputs.when }}"}), nil)
	checkpoint, err := Initialize("run-wait-fail", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	failed, err := Apply(checkpoint, definition, observedWithError(command, "wait 'hold': until: \"soon\" is not a time"))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RunFailed || failed.Failure == nil || failed.Failure.NodeID != "hold" {
		t.Fatalf("failed = %#v", failed)
	}
}

// A wait for a signal is still refused, and so is one with nothing to wait for.
func TestSignalWaitStaysIneligible(t *testing.T) {
	if !hasCode(CheckEligibility(waitTemplate(model.WaitConfig{Duration: "1h", Signal: "go"})), "unsupported_wait") {
		t.Fatal("a signal wait is refused")
	}
}
