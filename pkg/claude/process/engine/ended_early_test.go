package engine

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// giveUpTrackTemplate runs two tracks alongside each other; track a may give
// up into a failed end while track b still works:
//
//	start -> fork -{a}-> decide-a -{ok}-> join (all) -> end
//	                              -{give-up}-> gave-up (end, failed)
//	              -{b}-> work-b ----------->
//	              -{c}-> decide-c -{ok}---->
func giveUpTrackTemplate(result string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "give-up-track", Start: "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":     {Type: model.NodeTypeParallel, Next: model.Next{"a": "decide-a", "b": "work-b", "c": "decide-c"}},
			"decide-a": humanDecision("Go on?", model.Next{"ok": "join", "give-up": "gave-up"}),
			"work-b":   programTask("join", "work-b"),
			"decide-c": humanDecision("Fine?", model.Next{"ok": "join"}),
			"join":     joinAllTask("end", "join"),
			"end":      {Type: model.NodeTypeEnd},
			"gave-up":  {Type: model.NodeTypeEnd, Result: result},
		},
	}
}

func TestFailedEndInATrackEndsTheRunEarly(t *testing.T) {
	for _, tc := range []struct {
		result string
		status RunStatus
	}{{"failed", RunFailed}, {"canceled", RunCanceled}} {
		t.Run(tc.result, func(t *testing.T) {
			definition := mustPrepare(t, giveUpTrackTemplate(tc.result), nil)
			checkpoint, err := Initialize("run-give-up", definition)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, command := advanceAndPlan(t, checkpoint, definition)
			if command == nil || command.NodeID != "work-b" || len(checkpoint.AwaitingDecisions) != 2 {
				t.Fatalf("command = %#v, awaiting = %#v", command, checkpoint.AwaitingDecisions)
			}
			// Track a gives up while track b's program runs and track c waits on
			// a person.
			if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide-a", "give-up", 0)); err != nil {
				t.Fatal(err)
			}
			checkpoint, next := advanceAndPlan(t, checkpoint, definition)
			if next != nil {
				t.Fatalf("planned %#v in an ended run", next)
			}
			checkpoint = roundTrip(t, checkpoint, definition)
			if checkpoint.EndedEarly != "gave-up" || checkpoint.Nodes["gave-up"] != NodeDone || !Draining(checkpoint) {
				t.Fatalf("endedEarly = %q, nodes = %v", checkpoint.EndedEarly, checkpoint.Nodes)
			}
			if checkpoint.Status != RunRunning || len(checkpoint.AwaitingDecisions) != 0 {
				t.Fatalf("status = %q, awaiting = %#v: the run drains its program first", checkpoint.Status, checkpoint.AwaitingDecisions)
			}
			if checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0)); err != nil {
				t.Fatal(err)
			}
			checkpoint = roundTrip(t, checkpoint, definition)
			if checkpoint.Status != tc.status || checkpoint.Nodes["join"] == NodeDone || checkpoint.Nodes["end"] == NodeDone {
				t.Fatalf("status = %q, nodes = %v", checkpoint.Status, checkpoint.Nodes)
			}
		})
	}
}

// The other track already finished: the join is ready on its arrival alone,
// but a track gave up, so the join never runs.
func TestFailedEndInATrackStopsAReadyJoin(t *testing.T) {
	tmpl := giveUpTrackTemplate("failed")
	tmpl.Nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: model.Next{"a": "decide-a", "c": "decide-c"}}
	delete(tmpl.Nodes, "work-b")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-give-up-last", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide-c", "ok", 0)); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide-a", "give-up", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	checkpoint = roundTrip(t, checkpoint, definition)
	if checkpoint.Status != RunFailed || checkpoint.Nodes["join"] == NodeDone || checkpoint.Nodes["end"] != NodePending {
		t.Fatalf("status = %q, endedEarly = %q, nodes = %v", checkpoint.Status, checkpoint.EndedEarly, checkpoint.Nodes)
	}
}

func TestEndedEarlyMustNameAReachedFailedEnd(t *testing.T) {
	definition := mustPrepare(t, giveUpTrackTemplate("failed"), nil)
	checkpoint, err := Initialize("run-bad-ended", definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"end", "gave-up", "nope"} {
		bad := checkpoint
		bad.EndedEarly = id
		if err := ValidateCheckpoint(bad, definition); err == nil {
			t.Fatalf("endedEarly %q accepted", id)
		}
	}
}
