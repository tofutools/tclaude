package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// joinAnyTemplate is the shape TCL-715 exists for: several branches race, the
// first one to arrive activates the reducer's downstream route, and the losing
// branches keep running to their own settled outcome.
//
//	start -> fork -{bN}-> bN -> join(any) -> end
func joinAnyTemplate(branches ...string) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
		"join":  joinAnyTask("end", "true"),
		"end":   {Type: model.NodeTypeEnd},
	}
	next := model.Next{}
	for _, branch := range branches {
		next[branch] = branch
		nodes[branch] = programTask("join", "true")
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: next}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "join-any", Start: "start", Nodes: nodes,
	}
}

// directJoinAnyTemplate wires two fork branches STRAIGHT into the reducer, so
// one settlement pass settles both arrivals before either target is dequeued.
// It is the only genuinely simultaneous race the reducer can see.
func directJoinAnyTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "direct-join-any", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"a": "join", "b": "join"}},
			"join":  joinAnyTask("end", "true"),
			"end":   {Type: model.NodeTypeEnd},
		},
	}
}

func joinAnyTask(next, run string) model.Node {
	node := programTask(next, run)
	node.Join = model.JoinAny
	return node
}

// TestJoinAnyFirstArrivalWinsAndDownstreamActivatesOnce walks the whole slice in
// one run: the first arrival wins and activates the reducer, the losers keep
// running, their late arrivals settle honestly without touching the winner, the
// run refuses to call itself complete until they are done, and the last of them
// is what finalizes it.
func TestJoinAnyFirstArrivalWinsAndDownstreamActivatesOnce(t *testing.T) {
	definition := mustPrepare(t, joinAnyTemplate("fast", "slow-a", "slow-b"), nil)
	checkpoint, err := Initialize("run-join-any", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if fanned.Nodes["join"] != NodePending {
		t.Fatalf("join activated before any branch arrived: %q", fanned.Nodes["join"])
	}

	// Every branch is dispatched for real before anybody wins.
	dispatched, commands := planAll(t, fanned, definition)
	if len(commands) != 3 {
		t.Fatalf("planned %d branch commands, want 3", len(commands))
	}

	// The first arrival wins.
	won, err := Apply(dispatched, definition, observed(commands["fast"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if won.Nodes["join"] != NodeReady {
		t.Fatalf("first arrival did not activate the join: %q", won.Nodes["join"])
	}
	if won.Edges["fast"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("winning edge = %q", won.Edges["fast"][model.DefaultOutcome])
	}
	if won.Nodes["slow-a"] != NodeRunning || won.Nodes["slow-b"] != NodeRunning {
		t.Fatalf("losing branches were disturbed: %#v", won.Nodes)
	}
	if len(won.Commands) != 2 {
		t.Fatalf("losing commands = %#v; a winner must not cancel or drop them", won.Commands)
	}

	// The winner's downstream route runs, exactly once.
	downstream, joinCommand := advanceAndPlan(t, won, definition)
	if joinCommand == nil || joinCommand.NodeID != "join" {
		t.Fatalf("join was not planned after winning: %#v", joinCommand)
	}
	joined, err := Apply(downstream, definition, observed(joinCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if joined.Nodes["join"] != NodeDone || joined.Nodes["end"] != NodeReady {
		t.Fatalf("winner path did not reach the end: %#v", joined.Nodes)
	}

	// The end is reached, but losing work is still outstanding, so the run must
	// not claim to be over — and there is nothing left for the engine to commit.
	waiting, err := AdvanceUntilQuiescent(joined, definition)
	if err != nil {
		t.Fatalf("a ready end beside live losers must wait, not fail: %v", err)
	}
	if waiting.Status != RunRunning || waiting.Nodes["end"] != NodeReady {
		t.Fatalf("run completed while losers were still executing: status %q, end %q",
			waiting.Status, waiting.Nodes["end"])
	}
	if Runnable(waiting, definition) {
		t.Fatal("a run parked on its losers reported committable work")
	}

	// A late loser settles its own branch as honest evidence and changes nothing
	// about the winner.
	late, err := Apply(waiting, definition, observed(commands["slow-a"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if late.Edges["slow-a"][model.DefaultOutcome] != EdgeArrivedLate {
		t.Fatalf("late arrival = %q, want %q", late.Edges["slow-a"][model.DefaultOutcome], EdgeArrivedLate)
	}
	if late.Edges["fast"][model.DefaultOutcome] != EdgeArrived {
		t.Fatal("a late arrival replaced the durable winner")
	}
	if late.Nodes["join"] != NodeDone || late.Nodes["slow-a"] != NodeDone {
		t.Fatalf("late arrival left %#v", late.Nodes)
	}
	stillWaiting, err := AdvanceUntilQuiescent(late, definition)
	if err != nil {
		t.Fatal(err)
	}
	if stillWaiting.Status != RunRunning {
		t.Fatalf("run finalized with one loser still executing: %q", stillWaiting.Status)
	}

	// The last loser is what lets the run finalize.
	drained, err := Apply(stillWaiting, definition, observed(commands["slow-b"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := AdvanceUntilQuiescent(drained, definition)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RunCompleted {
		t.Fatalf("run status = %q after the last loser settled", completed.Status)
	}
	if _, planned, _, err := AdvanceAndPlan(completed, definition); err != nil || planned != nil {
		t.Fatalf("terminal run planned more work: %#v (%v)", planned, err)
	}
	arrived := 0
	for _, source := range []string{"fast", "slow-a", "slow-b"} {
		if completed.Edges[source][model.DefaultOutcome] == EdgeArrived {
			arrived++
		}
	}
	if arrived != 1 {
		t.Fatalf("%d edges claim to have won the join; exactly one may", arrived)
	}
}

// TestJoinAnySimultaneousArrivalsElectExactlyOneWinner covers the one race the
// serialized owner cannot separate: two edges settling inside the SAME
// settlement pass. The winner is the deterministically first authored edge and
// the other is late — never two winners, never a second activation.
func TestJoinAnySimultaneousArrivalsElectExactlyOneWinner(t *testing.T) {
	definition := mustPrepare(t, directJoinAnyTemplate(), nil)
	checkpoint, err := Initialize("run-direct-join-any", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if fanned.Edges["fork"]["a"] != EdgeArrived {
		t.Fatalf("fork/a = %q, want the deterministic winner %q", fanned.Edges["fork"]["a"], EdgeArrived)
	}
	if fanned.Edges["fork"]["b"] != EdgeArrivedLate {
		t.Fatalf("fork/b = %q, want %q", fanned.Edges["fork"]["b"], EdgeArrivedLate)
	}
	if fanned.Nodes["join"] != NodeReady {
		t.Fatalf("join = %q; the winner must have activated it exactly once", fanned.Nodes["join"])
	}
	// One winner means one activation means one command, not two.
	planned, commands := planAll(t, fanned, definition)
	if len(commands) != 1 || commands["join"] == nil {
		t.Fatalf("planned %#v; two winners would have planned the reducer twice", commands)
	}
	if planned.Nodes["join"] != NodeRunning {
		t.Fatalf("join = %q after planning", planned.Nodes["join"])
	}
}

// TestJoinAnyLateArrivalCannotReactivateTheReducer states the refusal directly
// rather than only through a whole run: once a join: any reducer has completed,
// nothing a losing branch reports can make it run again.
func TestJoinAnyLateArrivalCannotReactivateTheReducer(t *testing.T) {
	definition := mustPrepare(t, joinAnyTemplate("fast", "slow"), nil)
	checkpoint, err := Initialize("run-join-any-late", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, commands := planAll(t, fanned, definition)
	won, err := Apply(dispatched, definition, observed(commands["fast"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	downstream, joinCommand := advanceAndPlan(t, won, definition)
	joined, err := Apply(downstream, definition, observed(joinCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}

	late, err := Apply(joined, definition, observed(commands["slow"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if late.Nodes["join"] != NodeDone {
		t.Fatalf("late arrival moved the completed reducer to %q", late.Nodes["join"])
	}
	// The reducer's own outgoing route settled once, when the winner ran it. A
	// second activation would have to settle it again, which fails closed.
	if late.Edges["join"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("join outgoing = %q", late.Edges["join"][model.DefaultOutcome])
	}
	if _, err := Apply(late, definition, advanced("join")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a settled reducer accepted another advance: %v", err)
	}
	// And the duplicate observation a confused caller might retry is still stale.
	if _, err := Apply(late, definition, observed(commands["slow"], ProgramSucceeded, 0)); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("duplicate late observation = %v", err)
	}
}

// TestJoinAnyRestartPreservesTheWinnerAndInventsNoWork is the restart case: the
// durable state alone has to say who won, without re-electing anybody or
// deciding that the losers now need reconciling.
func TestJoinAnyRestartPreservesTheWinnerAndInventsNoWork(t *testing.T) {
	definition := mustPrepare(t, joinAnyTemplate("fast", "slow-a", "slow-b"), nil)
	checkpoint, err := Initialize("run-join-any-restart", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, commands := planAll(t, fanned, definition)
	won, err := Apply(dispatched, definition, observed(commands["fast"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(won)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatalf("a checkpoint holding a decided join: any must load: %v", err)
	}
	if reloaded.Edges["fast"][model.DefaultOutcome] != EdgeArrived ||
		reloaded.Nodes["join"] != NodeReady {
		t.Fatalf("restart lost the winner: %#v / %#v", reloaded.Edges, reloaded.Nodes)
	}

	// Recovery re-elects nothing: the reducer is already active, so the pass
	// plans the winner's downstream route and leaves both losers alone.
	resumed, planned, _, err := AdvanceAndPlan(reloaded, definition)
	if err != nil {
		t.Fatal(err)
	}
	if planned == nil || planned.NodeID != "join" {
		t.Fatalf("restart planned %#v, want the winner's downstream route", planned)
	}
	if resumed.Nodes["slow-a"] != NodeRunning || resumed.Nodes["slow-b"] != NodeRunning {
		t.Fatalf("restart disturbed the losing branches: %#v", resumed.Nodes)
	}
	if len(resumed.Commands) != 3 {
		t.Fatalf("commands = %d; restart must neither drop nor duplicate loser work", len(resumed.Commands))
	}
	if resumed.Edges["slow-a"][model.DefaultOutcome] != EdgeUnresolved {
		t.Fatal("restart settled a loser edge nobody observed")
	}
}

// TestJoinAnyClosesWhenNoBranchEverArrives keeps the zero-arrival rule: a
// join: any whose whole candidate set closes is still skipped, and its closure
// still propagates. Only the ARRIVAL rule changes.
func TestJoinAnyClosesWhenNoBranchEverArrives(t *testing.T) {
	tmpl := joinAnyTemplate("left", "right")
	tmpl.Nodes["start"] = model.Node{Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "choose"}}
	tmpl.Nodes["choose"] = humanDecision("Fan out?", model.Next{"go": "fork", "stop": "end"})
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-join-any-skip", definition)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := Apply(advanced, definition, Transition{
		Kind: TransitionDecisionRecorded, Decision: &DecisionRecord{NodeID: "choose", Verdict: "stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"fork", "left", "right", "join"} {
		if stopped.Nodes[nodeID] != NodeSkipped {
			t.Fatalf("node %q = %q, want skipped", nodeID, stopped.Nodes[nodeID])
		}
	}
	completed, err := AdvanceUntilQuiescent(stopped, definition)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RunCompleted {
		t.Fatalf("closed join: any run status = %q", completed.Status)
	}
}

// TestJoinArrivalsDerivesEvidenceForCommittedArrivalsOnly pins the evidence
// seam: it reports what one commit settled at a join: any reducer, in prepared
// order, and nothing else.
func TestJoinArrivalsDerivesEvidenceForCommittedArrivalsOnly(t *testing.T) {
	definition := mustPrepare(t, joinAnyTemplate("fast", "slow"), nil)
	checkpoint, err := Initialize("run-join-any-evidence", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if arrivals := definition.JoinArrivals(checkpoint, fanned); len(arrivals) != 0 {
		t.Fatalf("fan-out alone produced join arrivals: %#v", arrivals)
	}
	dispatched, commands := planAll(t, fanned, definition)
	won, err := Apply(dispatched, definition, observed(commands["fast"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	arrivals := definition.JoinArrivals(dispatched, won)
	if len(arrivals) != 1 || arrivals[0] != (JoinArrival{JoinNodeID: "join", From: "fast", Outcome: model.DefaultOutcome, Winner: true}) {
		t.Fatalf("winning arrival evidence = %#v", arrivals)
	}
	downstream, joinCommand := advanceAndPlan(t, won, definition)
	joined, err := Apply(downstream, definition, observed(joinCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	late, err := Apply(joined, definition, observed(commands["slow"], ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	arrivals = definition.JoinArrivals(joined, late)
	if len(arrivals) != 1 || arrivals[0] != (JoinArrival{JoinNodeID: "join", From: "slow", Outcome: model.DefaultOutcome}) {
		t.Fatalf("late arrival evidence = %#v", arrivals)
	}
	// A definition without a join: any reducer never walks anything.
	joinAll := mustPrepare(t, fanOutTemplate("left", "right"), nil)
	if arrivals := joinAll.JoinArrivals(Checkpoint{}, Checkpoint{}); arrivals != nil {
		t.Fatalf("join: all definition produced arrivals: %#v", arrivals)
	}
}

// planAll plans every currently ready program task, exactly as a bounded
// concurrent driver with spare capacity would, and returns their commands by
// node id.
func planAll(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, map[string]*Command) {
	t.Helper()
	commands := map[string]*Command{}
	for range len(definition.nodes) {
		next, command := advanceAndPlan(t, checkpoint, definition)
		checkpoint = next
		if command == nil {
			return checkpoint, commands
		}
		commands[command.NodeID] = command
	}
	t.Fatal("planning did not settle within the prepared node budget")
	return checkpoint, commands
}

// BenchmarkJoinArrivals measures the evidence walk that TCL-715 adds to every
// durable commit, at both ends of the range: a template with no join: any
// reducer at all, and a maximal-width fork reducing at one. It exists to keep
// the recorded yellow flag measurable rather than asserted.
func BenchmarkJoinArrivals(b *testing.B) {
	for _, width := range []int{0, 2, 64, 512} {
		name := fmt.Sprintf("width=%d", width)
		if width == 0 {
			name = "no-join-any"
		}
		b.Run(name, func(b *testing.B) {
			var tmpl *model.Template
			if width == 0 {
				tmpl = fanOutTemplate("left", "right")
			} else {
				branches := make([]string, 0, width)
				for i := range width {
					branches = append(branches, fmt.Sprintf("branch%03d", i))
				}
				tmpl = joinAnyTemplate(branches...)
			}
			definition, err := Prepare(tmpl, nil)
			if err != nil {
				b.Fatal(err)
			}
			before, err := Initialize("run-join-any-bench", definition)
			if err != nil {
				b.Fatal(err)
			}
			after, err := AdvanceUntilQuiescent(before, definition)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = definition.JoinArrivals(before, after)
			}
		})
	}
}
