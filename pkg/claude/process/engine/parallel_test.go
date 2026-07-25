package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// nestedFanOutTemplate nests a fan-out inside one branch of another:
//
//	start -> outer -{a}-> outer-task ------------------> outer-join -> end
//	               -{b}-> inner -{c}-> inner-c \
//	                            -{d}-> inner-d -> inner-join ------->
//
// Both joins are join: all, and each fork reduces at exactly one of them.
func nestedFanOutTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "nested-fan-out", Start: "start",
		Nodes: map[string]model.Node{
			"start":      {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "outer"}},
			"outer":      {Type: model.NodeTypeParallel, Next: model.Next{"a": "outer-task", "b": "inner"}},
			"outer-task": programTask("outer-join", "true"),
			"inner":      {Type: model.NodeTypeParallel, Next: model.Next{"c": "inner-c", "d": "inner-d"}},
			"inner-c":    programTask("inner-join", "true"),
			"inner-d":    programTask("inner-join", "true"),
			"inner-join": joinAllTask("outer-join", "true"),
			"outer-join": joinAllTask("end", "true"),
			"end":        {Type: model.NodeTypeEnd},
		},
	}
}

// decisionBranchTemplate parks each branch of a fork on its own human decision,
// with both verdicts of each decision routing into the shared join.
//
//	start -> fork -{a}-> decide-a -{yes,no}-> join(all) -> end
//	              -{b}-> decide-b -{yes,no}->
func decisionBranchTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "decision-branches", Start: "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":     {Type: model.NodeTypeParallel, Next: model.Next{"a": "decide-a", "b": "decide-b"}},
			"decide-a": humanDecision("A?", model.Next{"yes": "join", "no": "join"}),
			"decide-b": humanDecision("B?", model.Next{"yes": "join", "no": "join"}),
			"join":     joinAllTask("end", "true"),
			"end":      {Type: model.NodeTypeEnd},
		},
	}
}

// mixedBranchTemplate parks one branch on a human decision while the other
// branch runs a program task.
func mixedBranchTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "mixed-branches", Start: "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":     {Type: model.NodeTypeParallel, Next: model.Next{"a": "decide-a", "b": "task-b"}},
			"decide-a": humanDecision("A?", model.Next{"yes": "join", "no": "join"}),
			"task-b":   programTask("join", "true"),
			"join":     joinAllTask("end", "true"),
			"end":      {Type: model.NodeTypeEnd},
		},
	}
}

// skipThroughJoinTemplate puts a whole fan-out behind an exclusive decision, so
// choosing the other verdict closes the fork, both branches, and the join.
func skipThroughJoinTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "skip-through-join", Start: "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "choose"}},
			"choose": humanDecision("Fan out?", model.Next{"go": "fork", "stop": "merge"}),
			"fork":   {Type: model.NodeTypeParallel, Next: model.Next{"a": "left", "b": "right"}},
			"left":   programTask("join", "true"),
			"right":  programTask("join", "true"),
			"join":   joinAllTask("merge", "true"),
			"merge":  programTask("end", "true"),
			"end":    {Type: model.NodeTypeEnd},
		},
	}
}

// TestFanOutActivatesEveryBranchAndJoinAllReducesThem is the base case: one
// parallel advance settles every authored branch as arrived, both branches run
// independently, and the join: all reducer activates only once its complete
// candidate set has settled.
func TestFanOutActivatesEveryBranchAndJoinAllReducesThem(t *testing.T) {
	definition := mustPrepare(t, fanOutTemplate("left", "right"), nil)
	checkpoint, err := Initialize("run-fanout", definition)
	if err != nil {
		t.Fatal(err)
	}

	// One quiescent pass advances start and the fork, leaving BOTH branches ready.
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if fanned.Nodes["left"] != NodeReady || fanned.Nodes["right"] != NodeReady {
		t.Fatalf("fan-out did not activate both branches: %#v", fanned.Nodes)
	}
	if fanned.Edges["fork"]["left"] != EdgeArrived || fanned.Edges["fork"]["right"] != EdgeArrived {
		t.Fatalf("fork did not settle every branch as arrived: %#v", fanned.Edges["fork"])
	}
	if fanned.Nodes["join"] != NodePending {
		t.Fatalf("join activated before its candidate set settled: %q", fanned.Nodes["join"])
	}

	// Plan and observe the first branch. The join must keep waiting: its
	// candidate set is not complete while the second branch is unresolved.
	afterLeft := observeNext(t, fanned, definition, "left")
	if afterLeft.Nodes["join"] != NodePending {
		t.Fatalf("join activated on a partial candidate set: %q", afterLeft.Nodes["join"])
	}
	if afterLeft.Nodes["right"] != NodeReady {
		t.Fatalf("settling one branch disturbed the other: %q", afterLeft.Nodes["right"])
	}

	// The second arrival completes the candidate set, so join: all activates.
	afterRight := observeNext(t, afterLeft, definition, "right")
	if afterRight.Nodes["join"] != NodeReady {
		t.Fatalf("join did not activate on a complete candidate set: %q", afterRight.Nodes["join"])
	}

	completed := observeNext(t, afterRight, definition, "join")
	completed, _ = advanceAndPlan(t, completed, definition)
	if completed.Status != RunCompleted {
		t.Fatalf("fan-out run did not complete: %#v", completed)
	}
	for nodeID, status := range completed.Nodes {
		if status != NodeDone {
			t.Fatalf("terminal node %q = %q", nodeID, status)
		}
	}
}

// TestNestedFanOutReducesInnerScopeBeforeOuter drives a fan-out nested inside a
// branch of another fan-out to completion.
func TestNestedFanOutReducesInnerScopeBeforeOuter(t *testing.T) {
	definition := mustPrepare(t, nestedFanOutTemplate(), nil)
	checkpoint, err := Initialize("run-nested", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	// Both forks are engine-owned, so one pass opens the outer branch and the
	// whole inner scope: three tasks are ready at once.
	for _, nodeID := range []string{"outer-task", "inner-c", "inner-d"} {
		if fanned.Nodes[nodeID] != NodeReady {
			t.Fatalf("nested fan-out left %q = %q", nodeID, fanned.Nodes[nodeID])
		}
	}

	current := observeNext(t, fanned, definition, "inner-c")
	if current.Nodes["inner-join"] != NodePending {
		t.Fatalf("inner join activated early: %q", current.Nodes["inner-join"])
	}
	current = observeNext(t, current, definition, "inner-d")
	if current.Nodes["inner-join"] != NodeReady {
		t.Fatalf("inner join did not reduce its scope: %q", current.Nodes["inner-join"])
	}
	current = observeNext(t, current, definition, "inner-join")
	if current.Nodes["outer-join"] != NodePending {
		t.Fatalf("outer join activated before the outer branch arrived: %q", current.Nodes["outer-join"])
	}
	current = observeNext(t, current, definition, "outer-task")
	if current.Nodes["outer-join"] != NodeReady {
		t.Fatalf("outer join did not reduce its scope: %q", current.Nodes["outer-join"])
	}
	current = observeNext(t, current, definition, "outer-join")
	current, _ = advanceAndPlan(t, current, definition)
	if current.Status != RunCompleted {
		t.Fatalf("nested run did not complete: %#v", current)
	}
}

// TestWideFanOutRunsEveryBranchToCompletion is the wide smoke: a fork with many
// branches activates all of them and the join reduces only after the last one.
func TestWideFanOutRunsEveryBranchToCompletion(t *testing.T) {
	const width = 24
	definition := mustPrepare(t, wideFanOutTemplate(width), nil)
	checkpoint, err := Initialize("run-wide", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, status := range fanned.Nodes {
		if status == NodeReady {
			ready++
		}
	}
	if ready != width {
		t.Fatalf("wide fan-out activated %d branches, want %d", ready, width)
	}

	current := fanned
	for i := range width {
		branch := fmt.Sprintf("branch%02d", i)
		current = observeNext(t, current, definition, branch)
		wantJoin := NodePending
		if i == width-1 {
			wantJoin = NodeReady
		}
		if current.Nodes["join"] != wantJoin {
			t.Fatalf("after %d/%d branches the join = %q, want %q", i+1, width, current.Nodes["join"], wantJoin)
		}
	}
	current = observeNext(t, current, definition, "join")
	current, _ = advanceAndPlan(t, current, definition)
	if current.Status != RunCompleted {
		t.Fatalf("wide run did not complete: %#v", current)
	}
}

// TestBranchesCarryDecisionAndCommandSimultaneously proves the two outboxes are
// genuinely per-branch: one branch is parked on a human while another has a
// program command outstanding.
func TestBranchesCarryDecisionAndCommandSimultaneously(t *testing.T) {
	definition := mustPrepare(t, mixedBranchTemplate(), nil)
	checkpoint, err := Initialize("run-mixed", definition)
	if err != nil {
		t.Fatal(err)
	}
	current, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "task-b" {
		t.Fatalf("planned command = %#v, want task-b", command)
	}
	if !hasObligation(current, "decide-a") {
		t.Fatalf("decision branch lost its obligation: %#v", current.AwaitingDecisions)
	}
	if current.Nodes["decide-a"] != NodeReady || current.Nodes["task-b"] != NodeRunning {
		t.Fatalf("branch statuses = %#v", current.Nodes)
	}

	// Resolving either one must not disturb the other branch's outbox entry.
	afterDecision, err := Apply(current, definition, decided("decide-a", "yes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDecision.Commands) != 1 || afterDecision.Commands[0].NodeID != "task-b" {
		t.Fatalf("recording a decision disturbed the sibling command: %#v", afterDecision.Commands)
	}
	if len(afterDecision.AwaitingDecisions) != 0 {
		t.Fatalf("obligation survived its verdict: %#v", afterDecision.AwaitingDecisions)
	}

	afterProgram, err := Apply(current, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !hasObligation(afterProgram, "decide-a") {
		t.Fatalf("observing a program disturbed the sibling obligation: %#v", afterProgram.AwaitingDecisions)
	}

	// Either order reaches the same join activation.
	final, err := Apply(afterDecision, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if final.Nodes["join"] != NodeReady {
		t.Fatalf("join did not activate after both branches settled: %q", final.Nodes["join"])
	}
}

// TestPerEntryObligationRemovalKeepsSiblingBranchesAwaited covers plural
// decision obligations: two branches await decisions at once, each verdict is
// addressed individually, and resolving one leaves the other untouched.
func TestPerEntryObligationRemovalKeepsSiblingBranchesAwaited(t *testing.T) {
	definition := mustPrepare(t, decisionBranchTemplate(), nil)
	checkpoint, err := Initialize("run-two-decisions", definition)
	if err != nil {
		t.Fatal(err)
	}
	current, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.AwaitingDecisions) != 2 {
		t.Fatalf("expected one obligation per branch, got %#v", current.AwaitingDecisions)
	}
	// Sorted by node id, so a given logical state has one durable encoding.
	if current.AwaitingDecisions[0].NodeID != "decide-a" || current.AwaitingDecisions[1].NodeID != "decide-b" {
		t.Fatalf("obligations are not in deterministic order: %#v", current.AwaitingDecisions)
	}

	afterA, err := Apply(current, definition, decided("decide-a", "yes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterA.AwaitingDecisions) != 1 || afterA.AwaitingDecisions[0].NodeID != "decide-b" {
		t.Fatalf("one verdict cleared the sibling obligation: %#v", afterA.AwaitingDecisions)
	}
	if afterA.Nodes["join"] != NodePending {
		t.Fatalf("join activated before the second branch decided: %q", afterA.Nodes["join"])
	}
	// A duplicate or stale verdict for the already decided branch is refused
	// without touching the branch that is still awaited.
	if _, err := Apply(afterA, definition, decided("decide-a", "yes")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("duplicate verdict error = %v", err)
	}
	if _, err := Apply(afterA, definition, decided("decide-a", "no")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("late conflicting verdict error = %v", err)
	}
	if _, err := Apply(afterA, definition, decided("join", "yes")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("wrong-node verdict error = %v", err)
	}

	afterB, err := Apply(afterA, definition, decided("decide-b", "no"))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterB.AwaitingDecisions) != 0 {
		t.Fatalf("obligations survived both verdicts: %#v", afterB.AwaitingDecisions)
	}
	if afterB.Nodes["join"] != NodeReady {
		t.Fatalf("join did not reduce after both branches decided: %q", afterB.Nodes["join"])
	}
}

// TestPerEntryCommandRemovalKeepsSiblingBranchCommand plans a command on each
// branch — the refill PR2 will do — and proves an observation removes only its
// own exact entry, while duplicate and stale observations are still refused.
func TestPerEntryCommandRemovalKeepsSiblingBranchCommand(t *testing.T) {
	definition := mustPrepare(t, fanOutTemplate("left", "right"), nil)
	checkpoint, err := Initialize("run-two-commands", definition)
	if err != nil {
		t.Fatal(err)
	}
	current, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	// Repeated planning refills the outbox one branch at a time without any
	// change to the durable shape: this is exactly the PR2 refill loop.
	current, leftCommand := advanceAndPlan(t, current, definition)
	current, rightCommand := advanceAndPlan(t, current, definition)
	if leftCommand == nil || rightCommand == nil || leftCommand.NodeID != "left" || rightCommand.NodeID != "right" {
		t.Fatalf("refill did not plan one command per branch: %#v / %#v", leftCommand, rightCommand)
	}
	if len(current.Commands) != 2 {
		t.Fatalf("command outbox = %#v, want one entry per branch", current.Commands)
	}
	if current.Commands[0].NodeID != "left" || current.Commands[1].NodeID != "right" {
		t.Fatalf("commands are not in deterministic order: %#v", current.Commands)
	}
	// Two commands at once is structurally safe durable state.
	if err := ValidateCheckpoint(current, definition); err != nil {
		t.Fatalf("boundary validator rejected two live branch commands: %v", err)
	}

	afterLeft, err := Apply(current, definition, observed(leftCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterLeft.Commands) != 1 || afterLeft.Commands[0].NodeID != "right" {
		t.Fatalf("observation removed more than its own command: %#v", afterLeft.Commands)
	}
	if afterLeft.Nodes["right"] != NodeRunning {
		t.Fatalf("sibling branch regressed to %q", afterLeft.Nodes["right"])
	}
	if _, err := Apply(afterLeft, definition, observed(leftCommand, ProgramSucceeded, 0)); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("duplicate observation error = %v", err)
	}
	forged := observed(rightCommand, ProgramSucceeded, 0)
	forged.Observation.CommandID = leftCommand.ID
	if _, err := Apply(afterLeft, definition, forged); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("mismatched command/node observation error = %v", err)
	}

	afterRight, err := Apply(afterLeft, definition, observed(rightCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRight.Commands) != 0 || afterRight.Nodes["join"] != NodeReady {
		t.Fatalf("final branch settlement = %#v", afterRight)
	}
}

// TestEngineOwnedAdvanceProceedsAlongsideBranchWork proves an engine-owned node
// advances while unrelated branches hold a command or an obligation. The old
// sequential loop stopped as soon as either outbox was non-empty, which would
// deadlock a fan-out.
func TestEngineOwnedAdvanceProceedsAlongsideBranchWork(t *testing.T) {
	t.Run("alongside an outstanding command", func(t *testing.T) {
		definition := mustPrepare(t, nestedFanOutTemplate(), nil)
		checkpoint, err := Initialize("run-advance-command", definition)
		if err != nil {
			t.Fatal(err)
		}
		// Stop right after the outer fork so the inner fork is still unadvanced.
		outerFanned, err := Apply(checkpoint, definition, advanced("start"))
		if err != nil {
			t.Fatal(err)
		}
		outerFanned, err = Apply(outerFanned, definition, advanced("outer"))
		if err != nil {
			t.Fatal(err)
		}
		outerCommand := programCommand(outerFanned.RunID, definition.nodes[definition.index["outer-task"]],
			nextAttempt(outerFanned, "outer-task"))
		withCommand, err := Apply(outerFanned, definition, Transition{Kind: TransitionCommandPlanned, Command: &outerCommand})
		if err != nil {
			t.Fatal(err)
		}
		if withCommand.Nodes["inner"] != NodeReady {
			t.Fatalf("inner fork = %q, want ready", withCommand.Nodes["inner"])
		}

		advancedState, err := AdvanceUntilQuiescent(withCommand, definition)
		if err != nil {
			t.Fatal(err)
		}
		if advancedState.Nodes["inner"] != NodeDone {
			t.Fatalf("engine-owned fork did not advance past a sibling command: %q", advancedState.Nodes["inner"])
		}
		if len(advancedState.Commands) != 1 || advancedState.Commands[0].NodeID != "outer-task" {
			t.Fatalf("advancing disturbed the sibling command: %#v", advancedState.Commands)
		}
	})

	t.Run("alongside an awaited decision", func(t *testing.T) {
		definition := mustPrepare(t, skipThroughJoinTemplate(), nil)
		checkpoint := advanceToDecision(t, "run-advance-decision", definition, "choose")
		// The verdict opens the fork, which is engine-owned, and the advance must
		// run it even though the run just came off a decision.
		next, err := Apply(checkpoint, definition, decided("choose", "go"))
		if err != nil {
			t.Fatal(err)
		}
		fanned, err := AdvanceUntilQuiescent(next, definition)
		if err != nil {
			t.Fatal(err)
		}
		if fanned.Nodes["left"] != NodeReady || fanned.Nodes["right"] != NodeReady {
			t.Fatalf("post-decision fan-out did not open both branches: %#v", fanned.Nodes)
		}
	})
}

// TestSkipPropagatesIntoAndThroughAJoin closes a whole fan-out with an upstream
// verdict: the fork, both branches, and the join all skip, and the closure keeps
// propagating past the join to the node beyond it.
func TestSkipPropagatesIntoAndThroughAJoin(t *testing.T) {
	definition := mustPrepare(t, skipThroughJoinTemplate(), nil)
	checkpoint := advanceToDecision(t, "run-skip-join", definition, "choose")

	next, err := Apply(checkpoint, definition, decided("choose", "stop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"fork", "left", "right", "join"} {
		if next.Nodes[nodeID] != NodeSkipped {
			t.Fatalf("closed fan-out left %q = %q, want skipped", nodeID, next.Nodes[nodeID])
		}
	}
	if next.Edges["join"]["next"] != EdgeNotTaken {
		t.Fatalf("the skipped join did not close its own outgoing edge: %#v", next.Edges["join"])
	}
	// The merge beyond the join still activates: its candidate set settled with
	// exactly the one arrival the chosen verdict produced.
	if next.Nodes["merge"] != NodeReady {
		t.Fatalf("merge past the skipped join = %q", next.Nodes["merge"])
	}

	current := observeNext(t, next, definition, "merge")
	current, _ = advanceAndPlan(t, current, definition)
	if current.Status != RunCompleted {
		t.Fatalf("run with a fully closed fan-out did not complete: %#v", current)
	}
}

// TestJoinAllAcceptsPartialArrivalsAfterAnInnerSkip settles a join whose
// candidate set contains both an arrival and a closed candidate: join: all
// activates on one or more arrivals, unlike the exactly-one non-join rule.
func TestJoinAllAcceptsPartialArrivalsAfterAnInnerSkip(t *testing.T) {
	definition := mustPrepare(t, decisionBranchTemplate(), nil)
	checkpoint, err := Initialize("run-partial-join", definition)
	if err != nil {
		t.Fatal(err)
	}
	current, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	current, err = Apply(current, definition, decided("decide-a", "yes"))
	if err != nil {
		t.Fatal(err)
	}
	current, err = Apply(current, definition, decided("decide-b", "no"))
	if err != nil {
		t.Fatal(err)
	}
	// Each decision arrived on one of its two outcomes, so the join's candidate
	// set holds two arrivals and two closed candidates.
	counts := countDispositions(current, definition, definition.nodes[definition.index["join"]].incoming)
	if counts.arrived != 2 || counts.notTaken != 2 || counts.unresolved != 0 {
		t.Fatalf("join candidate set = %+v", counts)
	}
	if current.Nodes["join"] != NodeReady {
		t.Fatalf("join: all did not accept multiple arrivals: %q", current.Nodes["join"])
	}
}

// TestNonJoinNodeRejectsMoreThanOneArrival keeps the local fail-closed rule as
// defense in depth behind the static reducer requirement: a convergence node
// that declared no join policy cannot absorb two arrivals.
//
// Authoring now refuses to let a parallel fork reduce at an unannotated node
// (see TestParallelForkReducerMustDeclareItsJoinPolicy), so this state is
// unreachable from any valid template. It is built directly from an exclusive
// decision merge — which legitimately stays unannotated, because at most one of
// its candidates can normally arrive — to prove the reducer still fails closed
// rather than silently picking one arrival.
func TestNonJoinNodeRejectsMoreThanOneArrival(t *testing.T) {
	definition := mustPrepare(t, decisionDiamondTemplate(), nil)
	base := advanceToDecision(t, "run-double-arrival", definition, "choose", "intake")
	next, err := Apply(base, definition, decided("choose", "approve"))
	if err != nil {
		t.Fatal(err)
	}
	if definition.nodes[definition.index["merge"]].joinAll {
		t.Fatal("fixture merge must be an unannotated exclusive merge")
	}

	// Force the state the static rule prevents: the closed branch is live again
	// and the chosen branch has already arrived at the merge.
	forced := cloneCheckpoint(next)
	forced.Nodes["fast"] = NodeDone
	forced.Edges["fast"]["next"] = EdgeArrived
	forced.Nodes["slow"] = NodeReady
	forced.Edges["slow"]["next"] = EdgeUnresolved

	err = completeAndSettle(&forced, definition, definition.index["slow"], arrivesAt(defaultEdgeOutcome(definition, "slow")))
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second arrival into a non-join node = %v, want ErrInvalidTransition", err)
	}
	// Several preconditions in completeAndSettle share this sentinel, so pin the
	// arrival guard specifically rather than accepting any refusal.
	if !strings.Contains(err.Error(), "received more than one arrival") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestParallelForkReducerMustDeclareItsJoinPolicy pins the one-time definition
// rule that makes the runtime guard above unreachable: the node the static
// parallel-scope analysis identifies as a fork's structural reducer has to
// declare how it reduces, and an unannotated one is refused before a run can
// ever be created. Ordinary exclusive merges are untouched.
func TestParallelForkReducerMustDeclareItsJoinPolicy(t *testing.T) {
	t.Run("unannotated fork reducer is refused", func(t *testing.T) {
		tmpl := fanOutTemplate("left", "right")
		join := tmpl.Nodes["join"]
		join.Join = ""
		tmpl.Nodes["join"] = join

		if !hasCode(CheckEligibility(tmpl), "missing_reducer_join") {
			t.Fatalf("unannotated fork reducer was admitted: %#v", CheckEligibility(tmpl))
		}
		if _, err := Prepare(tmpl, nil); !errors.Is(err, ErrTemplateIneligible) {
			t.Fatalf("Prepare error = %v, want an ineligible template", err)
		}
	})

	t.Run("declared join all is admitted", func(t *testing.T) {
		tmpl := fanOutTemplate("left", "right")
		if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
			t.Fatalf("declared join: all was refused: %#v", diagnostics)
		}
		if _, err := Prepare(tmpl, nil); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	})

	t.Run("exclusive decision merge stays unannotated", func(t *testing.T) {
		// merge has two inbound candidates and declares no join policy; it is a
		// decision merge, not a parallel fork's reducer.
		tmpl := decisionDiamondTemplate()
		if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
			t.Fatalf("exclusive merge was forced to declare a join: %#v", diagnostics)
		}
		if _, err := Prepare(tmpl, nil); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	})
}

// TestBranchFailureAbandonsSiblingOutboxEntries pins how a failing branch ends
// the run: the run fails exactly as it did sequentially, and no sibling
// obligation or command survives into terminal state, where it would read as
// still actionable and would be rejected by the load boundary.
func TestBranchFailureAbandonsSiblingOutboxEntries(t *testing.T) {
	definition := mustPrepare(t, mixedBranchTemplate(), nil)
	checkpoint, err := Initialize("run-branch-failure", definition)
	if err != nil {
		t.Fatal(err)
	}
	current, command := advanceAndPlan(t, checkpoint, definition)
	if !hasObligation(current, "decide-a") {
		t.Fatalf("fixture did not park the sibling branch on a decision: %#v", current)
	}

	failed, err := Apply(current, definition, observed(command, ProgramFailed, 3))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RunFailed || failed.Nodes["task-b"] != NodeFailed {
		t.Fatalf("branch failure did not fail the run: %#v", failed)
	}
	if len(failed.Commands) != 0 || len(failed.AwaitingDecisions) != 0 {
		t.Fatalf("terminal run kept live outbox entries: %#v", failed)
	}
	// The abandoned state must still survive the load/persist boundary.
	if err := ValidateCheckpoint(failed, definition); err != nil {
		t.Fatalf("boundary validator rejected the abandoned terminal state: %v", err)
	}
	if _, err := Apply(failed, definition, decided("decide-a", "yes")); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("terminal run accepted a verdict: %v", err)
	}
}

// TestEndRefusesToTerminateWhileBranchWorkRemains proves the terminal guard: a
// run never reports terminal while another branch is still live. Structured
// authoring makes this unreachable, so the state is built directly.
func TestEndRefusesToTerminateWhileBranchWorkRemains(t *testing.T) {
	definition := mustPrepare(t, fanOutTemplate("left", "right"), nil)
	checkpoint, err := Initialize("run-terminal-guard", definition)
	if err != nil {
		t.Fatal(err)
	}
	fanned, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	// A live sibling branch blocks termination.
	stranded := cloneCheckpoint(fanned)
	stranded.Nodes["end"] = NodeReady
	if _, err := Apply(stranded, definition, advanced("end")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("end advanced with live branches: %v", err)
	}

	// So does an outstanding command, with every other node quiet.
	withCommand, command := advanceAndPlan(t, fanned, definition)
	if command == nil {
		t.Fatal("fixture did not plan a branch command")
	}
	commandOnly := cloneCheckpoint(withCommand)
	for _, node := range definition.nodes {
		commandOnly.Nodes[node.id] = NodeDone
	}
	commandOnly.Nodes["end"] = NodeReady
	if _, err := Apply(commandOnly, definition, advanced("end")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("end advanced with an outstanding command: %v", err)
	}

	// And so does an unresolved obligation.
	obligationOnly := cloneCheckpoint(commandOnly)
	obligationOnly.Commands = nil
	addObligation(&obligationOnly, "left")
	if _, err := Apply(obligationOnly, definition, advanced("end")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("end advanced with an awaited obligation: %v", err)
	}
}

// observeNext plans the next command, requires it to be for the expected node,
// and applies a successful observation for it.
func observeNext(t *testing.T, checkpoint Checkpoint, definition *Definition, nodeID string) Checkpoint {
	t.Helper()
	next, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil {
		t.Fatalf("no command was planned; expected one for %q", nodeID)
	}
	if command.NodeID != nodeID {
		t.Fatalf("planned command for %q, want %q", command.NodeID, nodeID)
	}
	observedNext, err := Apply(next, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatalf("observe %q: %v", nodeID, err)
	}
	return observedNext
}
