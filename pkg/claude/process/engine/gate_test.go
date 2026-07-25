package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// gateTemplate is the rework fixture: start -> build -> end, where build is a
// compound with an authored retry budget, TWO check steps, and a review. The
// second check is what makes "a passed intervening gate runs again" observable
// rather than merely stated.
//
// maxAttempts of zero authors no retry policy at all, which is the fail-fast
// compound the same tests measure the retryable one against.
func gateTemplate(maxAttempts int) *model.Template {
	return compoundStageTemplate(func(node *model.Node) {
		if maxAttempts > 0 {
			node.Retry = &model.RetryPolicy{MaxAttempts: maxAttempts}
		}
		node.Checks = append(node.Checks, model.Step{ID: "lint",
			Performer: model.Performer{Kind: model.PerformerProgram, Profile: "worker", Run: "lint"}})
	})
}

// gateStageIDs is the fixture's exact expansion, in order.
var gateStageIDs = []string{
	"build.plan", "build.do", "build.test.unit", "build.test.lint", "build.review", "build.done",
}

// runStage plans the next command, asserts it is exactly the stage and attempt
// expected, and applies the given outcome. Naming the attempt at every step is
// what makes the single-budget accounting visible in the test itself.
func runStage(t *testing.T, checkpoint Checkpoint, definition *Definition,
	nodeID string, attempt int, outcome ProgramOutcome) Checkpoint {
	t.Helper()
	next, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != nodeID || command.Attempt != attempt {
		t.Fatalf("planned %#v, want node %q attempt %d", command, nodeID, attempt)
	}
	transition := observed(command, outcome, 0)
	if outcome == ProgramFailed {
		transition = observedWithError(command, nodeID+" rejected the work")
	}
	settled, err := Apply(next, definition, transition)
	if err != nil {
		t.Fatalf("observe %q attempt %d: %v", nodeID, attempt, err)
	}
	return settled
}

// assertStageReset checks the compact fact a transition input makes derivable
// for evidence, looked up through the gate that input named.
func assertStageReset(t *testing.T, definition *Definition, gateNodeID string,
	before, after Checkpoint, want StageReset) {
	t.Helper()
	got, ok := definition.StageReset(gateNodeID, before, after)
	if !ok {
		t.Fatalf("gate %q reported no stage reset", gateNodeID)
	}
	if got != want {
		t.Fatalf("stage reset\n got: %#v\nwant: %#v", got, want)
	}
}

// assertNodes compares the whole node-status map against an exact expectation.
func assertNodes(t *testing.T, checkpoint Checkpoint, want map[string]NodeStatus) {
	t.Helper()
	if !maps.Equal(checkpoint.Nodes, want) {
		t.Fatalf("node statuses\n got: %v\nwant: %v", checkpoint.Nodes, want)
	}
}

func TestCompoundRetryIsEligibleAndPreparesOneBudgetOnItsDoStage(t *testing.T) {
	tmpl := gateTemplate(3)
	assertAuthoringValid(t, tmpl)
	if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
		t.Fatalf("a supported compound retry was refused: %#v", diagnostics)
	}
	definition := mustPrepare(t, tmpl, nil)

	parent := definition.nodes[definition.index["build"]]
	stageIDs := make([]string, 0, len(parent.children))
	for _, childIndex := range parent.children {
		stageIDs = append(stageIDs, definition.nodes[childIndex].id)
	}
	if !slices.Equal(stageIDs, gateStageIDs) {
		t.Fatalf("derived stages\n got: %v\nwant: %v", stageIDs, gateStageIDs)
	}
	if got := definition.nodes[parent.doAnchor].id; got != "build.do" {
		t.Fatalf("prepared do anchor = %q", got)
	}
	// The authored budget lands on the do stage and nowhere else.
	if work := definition.nodes[parent.doAnchor]; work.maxAttempts != 3 || !work.retryAuthored {
		t.Fatalf("do stage budget = %d, authored = %v", work.maxAttempts, work.retryAuthored)
	}
	for _, stageID := range []string{"build.plan", "build.test.unit", "build.test.lint", "build.review"} {
		child := definition.nodes[definition.index[stageID]]
		if child.maxAttempts != 1 || child.retryAuthored {
			t.Fatalf("stage %q carries a budget of its own: %d / %v", stageID, child.maxAttempts, child.retryAuthored)
		}
	}

	checkpoint, err := Initialize("run-gate-budget", definition)
	if err != nil {
		t.Fatal(err)
	}
	// A gate's ceiling is DERIVED from the do anchor. The plan stage is not a
	// gate — a failed plan program stays fail-fast — so it derives nothing.
	for stageID, want := range map[string]int{
		"build.test.unit": 3, "build.test.lint": 3, "build.review": 3, "build.plan": 1, "build.do": 3,
	} {
		node := definition.nodes[definition.index[stageID]]
		if got := executableAttemptCeiling(checkpoint, definition, node); got != want {
			t.Fatalf("stage %q executable ceiling = %d, want %d", stageID, got, want)
		}
	}
}

// TestAuthoredPoisonCycleStaysIneligibleUnderCompoundRetry guards the door
// admitting a compound's retry policy could have looked like it opened.
//
// Authoring sanctions exactly one cycle — a compound's fail edge to a human
// decision whose retry edge points back at it — and this engine refuses it. It
// always did so through TWO independent rules, and the one that survives here
// is routing: that shape needs a second outgoing route on the compound, and a
// task in this engine keeps exactly one. The rework loop this slice adds is
// local to a prepared child list and is not that authored cycle.
func TestAuthoredPoisonCycleStaysIneligibleUnderCompoundRetry(t *testing.T) {
	build := compoundTask("end")
	build.Retry = &model.RetryPolicy{MaxAttempts: 2}
	build.Next = model.Next{model.DefaultOutcome: "end", "fail": "escalate"}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "poison", Start: "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "build"}},
			"build":    build,
			"escalate": humanDecision("Retry?", model.Next{"retry": "build", "cancel": "canceled"}),
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
			"end":      {Type: model.NodeTypeEnd},
		},
	}
	if !hasCode(CheckEligibility(tmpl), "unsupported_routing") {
		t.Fatalf("an authored poison cycle became eligible: %#v", CheckEligibility(tmpl))
	}
	if _, err := Prepare(tmpl, nil); !errors.Is(err, ErrTemplateIneligible) {
		t.Fatalf("prepare error = %v, want an ineligible template", err)
	}
}

// TestFailedCheckReRunsTheWorkAndEveryGateThenCompletes is the central loop: a
// rejected check sends the work back inside the compound's own budget, the
// already-passed check runs again over the replaced work, and the compound then
// finishes normally.
func TestFailedCheckReRunsTheWorkAndEveryGateThenCompletes(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(2), nil)
	checkpoint, err := Initialize("run-gate-check", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramSucceeded)
	before := cloneCheckpoint(checkpoint)
	checkpoint = runStage(t, checkpoint, definition, "build.test.lint", 1, ProgramFailed)

	// The exact local reset: the work is ready again, the passed check and the
	// failed gate are pending, the plan stage keeps its result, the stages after
	// the gate were already pending and stay that way, and the parent runs on.
	assertNodes(t, checkpoint, map[string]NodeStatus{
		"start": NodeDone, "build": NodeRunning, "end": NodePending,
		"build.plan": NodeDone, "build.do": NodeReady,
		"build.test.unit": NodePending, "build.test.lint": NodePending,
		"build.review": NodePending, "build.done": NodePending,
	})
	// Attempts are never reset or reused, and no gate earned a ceiling entry.
	if want := (map[string]int{
		"build.plan": 1, "build.do": 1, "build.test.unit": 1, "build.test.lint": 1,
	}); !reflect.DeepEqual(checkpoint.Attempts, want) {
		t.Fatalf("attempts after the reset\n got: %v\nwant: %v", checkpoint.Attempts, want)
	}
	if len(checkpoint.AttemptCeilings) != 0 || len(checkpoint.Blocked) != 0 || len(checkpoint.Commands) != 0 {
		t.Fatalf("the reset wrote durable state of its own: %#v", checkpoint)
	}
	if checkpoint.Edges["build"][model.DefaultOutcome] != EdgeUnresolved {
		t.Fatalf("the reset settled the parent's authored route: %q",
			checkpoint.Edges["build"][model.DefaultOutcome])
	}
	// One compact reset fact is derivable for evidence, from the gate the causing
	// input already named: which parent, which work, and the attempt it will
	// next carry.
	assertStageReset(t, definition, "build.test.lint", before, checkpoint, StageReset{
		ParentNodeID: "build", GateNodeID: "build.test.lint", WorkNodeID: "build.do", NextWorkAttempt: 2,
	})
	// The derivation is targeted and fail-closed: no other node of the same
	// transition claims the reset, including the work it re-readied.
	for _, nodeID := range []string{
		"build", "build.do", "build.plan", "build.test.unit", "build.review", "build.done",
		"start", "end", "nowhere",
	} {
		if reset, ok := definition.StageReset(nodeID, before, checkpoint); ok {
			t.Fatalf("node %q claimed the reset: %#v", nodeID, reset)
		}
	}

	// The second pass re-runs the work and BOTH gates, including the one that
	// already passed over work that no longer exists.
	checkpoint = runStage(t, checkpoint, definition, "build.do", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.lint", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.review", 1, ProgramSucceeded)

	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command != nil {
		t.Fatalf("planned past the finished compound: %#v", command)
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
	if checkpoint.Edges["build"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("parent edge = %q", checkpoint.Edges["build"][model.DefaultOutcome])
	}
	// The plan stage ran exactly once: a gate's verdict is about the work.
	if got := checkpoint.Attempts["build.plan"]; got != 1 {
		t.Fatalf("plan stage attempts = %d, want 1", got)
	}
}

// TestFailedReviewReRunsTheWorkAndEveryPassedCheck is the same rule at the last
// gate: a review rejects the work, so every check that passed over it runs
// again too.
func TestFailedReviewReRunsTheWorkAndEveryPassedCheck(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(2), nil)
	checkpoint, err := Initialize("run-gate-review", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.lint", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.review", 1, ProgramFailed)

	assertNodes(t, checkpoint, map[string]NodeStatus{
		"start": NodeDone, "build": NodeRunning, "end": NodePending,
		"build.plan": NodeDone, "build.do": NodeReady,
		"build.test.unit": NodePending, "build.test.lint": NodePending,
		"build.review": NodePending, "build.done": NodePending,
	})

	checkpoint = runStage(t, checkpoint, definition, "build.do", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.lint", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.review", 2, ProgramSucceeded)
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
}

// TestCompoundWithoutAuthoredRetryStaysFailFast pins the other half of the
// product rule: rework is authored policy. A compound that declared none keeps
// today's behaviour at its FIRST failure, wherever in the sequence it happens.
func TestCompoundWithoutAuthoredRetryStaysFailFast(t *testing.T) {
	for _, failAt := range []string{"build.plan", "build.do", "build.test.unit", "build.review"} {
		t.Run(failAt, func(t *testing.T) {
			definition := mustPrepare(t, gateTemplate(0), nil)
			checkpoint, err := Initialize("run-gate-fail-fast", definition)
			if err != nil {
				t.Fatal(err)
			}
			for _, stageID := range gateStageIDs {
				outcome := ProgramSucceeded
				if stageID == failAt {
					outcome = ProgramFailed
				}
				checkpoint = runStage(t, checkpoint, definition, stageID, 1, outcome)
				if stageID == failAt {
					break
				}
			}
			if checkpoint.Nodes[failAt] != NodeFailed {
				t.Fatalf("%q = %q, want failed", failAt, checkpoint.Nodes[failAt])
			}
			if len(checkpoint.Blocked) != 0 {
				t.Fatalf("a compound with no authored retry parked a branch: %#v", checkpoint.Blocked)
			}
			if checkpoint.Status != RunFailed {
				t.Fatalf("run status = %q, want failed", checkpoint.Status)
			}
			// Only the done stage completes a parent, exactly as before.
			if checkpoint.Nodes["build"] != NodeRunning {
				t.Fatalf("compound parent = %q", checkpoint.Nodes["build"])
			}
		})
	}
}

// TestDoFailureAndGateReworkSpendOneBudget is the single-budget rule measured
// directly: three do executions, one spent by the do program failing and one by
// a gate sending the work back, and the third is the last the compound gets.
func TestDoFailureAndGateReworkSpendOneBudget(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(3), nil)
	checkpoint, err := Initialize("run-gate-one-budget", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	// One execution spent by the do program's own failure: it re-readies in place.
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramFailed)
	if checkpoint.Nodes["build.do"] != NodeReady {
		t.Fatalf("do stage after its own failure = %q", checkpoint.Nodes["build.do"])
	}
	checkpoint = runStage(t, checkpoint, definition, "build.do", 2, ProgramSucceeded)
	// One spent by a gate sending the work back.
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramFailed)
	if checkpoint.Nodes["build.do"] != NodeReady {
		t.Fatalf("do stage after the gate rejected it = %q", checkpoint.Nodes["build.do"])
	}
	checkpoint = runStage(t, checkpoint, definition, "build.do", 3, ProgramSucceeded)
	// The third is the last: the same gate rejecting again has nothing to spend.
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 2, ProgramFailed)

	if checkpoint.Nodes["build.test.unit"] != NodeBlocked || checkpoint.Nodes["build.do"] != NodeDone {
		t.Fatalf("exhausted state = %v", checkpoint.Nodes)
	}
	if len(checkpoint.Blocked) != 1 || checkpoint.Blocked[0].NodeID != "build.test.unit" {
		t.Fatalf("blocked outbox = %#v", checkpoint.Blocked)
	}
	if checkpoint.Blocked[0].Reason != "build.test.unit rejected the work" {
		t.Fatalf("blocked reason = %q", checkpoint.Blocked[0].Reason)
	}
	// The parked gate's exact identity is its OWN node and attempt.
	if got := checkpoint.Attempts["build.test.unit"]; got != 2 {
		t.Fatalf("blocked gate attempt = %d, want 2", got)
	}
	if got := checkpoint.Attempts["build.do"]; got != 3 {
		t.Fatalf("do attempts = %d, want the whole budget", got)
	}
	// No gate ever earns a ceiling entry: its bound is derived from the anchor.
	if len(checkpoint.AttemptCeilings) != 0 {
		t.Fatalf("attempt ceilings = %#v", checkpoint.AttemptCeilings)
	}
	// A parked branch is not a doomed one.
	if checkpoint.Status != RunRunning || Draining(checkpoint) || Runnable(checkpoint, definition) {
		t.Fatalf("exhausted run = %q, draining %v, runnable %v",
			checkpoint.Status, Draining(checkpoint), Runnable(checkpoint, definition))
	}
}

// parallelGateTemplate forks into two identical retryable compounds that reduce
// at one join: all, which is the shape every isolation property is measured on.
func parallelGateTemplate(maxAttempts int) *model.Template {
	compound := func() model.Node {
		node := compoundTask("join")
		if maxAttempts > 0 {
			node.Retry = &model.RetryPolicy{MaxAttempts: maxAttempts}
		}
		node.Checks = append(node.Checks, model.Step{ID: "lint",
			Performer: model.Performer{Kind: model.PerformerProgram, Profile: "worker", Run: "lint"}})
		return node
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-gate", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  compound(), "right": compound(),
			"join": joinAllTask("end", "true"),
			"end":  {Type: model.NodeTypeEnd},
		},
	}
}

// fillOutbox tops the outbox up the way a concurrent driver does — repeated
// AdvanceAndPlan calls until nothing more is plannable — and returns the whole
// DURABLE outbox by node id, including entries an earlier call planned, so a
// parallel run can be driven branch by branch in any order.
func fillOutbox(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, map[string]Command) {
	t.Helper()
	for range 4 * len(definition.nodes) {
		next, command := advanceAndPlan(t, checkpoint, definition)
		checkpoint = next
		if command != nil {
			continue
		}
		commands := make(map[string]Command, len(checkpoint.Commands))
		for _, outstanding := range checkpoint.Commands {
			commands[outstanding.NodeID] = outstanding
		}
		return checkpoint, commands
	}
	t.Fatal("planning did not settle within the prepared node budget")
	return checkpoint, nil
}

// settleStage tops the outbox up and applies one outcome to the named branch's
// outstanding command, leaving every other branch's exactly as it was.
func settleStage(t *testing.T, checkpoint Checkpoint, definition *Definition,
	nodeID string, outcome ProgramOutcome) Checkpoint {
	t.Helper()
	checkpoint, commands := fillOutbox(t, checkpoint, definition)
	command, ok := commands[nodeID]
	if !ok {
		t.Fatalf("no outstanding command for %q; outbox holds %v",
			nodeID, slices.Sorted(maps.Keys(commands)))
	}
	transition := observed(&command, outcome, 0)
	if outcome == ProgramFailed {
		transition = observedWithError(&command, nodeID+" rejected the work")
	}
	next, err := Apply(checkpoint, definition, transition)
	if err != nil {
		t.Fatalf("observe %q attempt %d: %v", nodeID, command.Attempt, err)
	}
	return next
}

// TestExhaustedGateParksOnlyItsOwnBranch proves a gate that ran out of budget is
// branch-local: the parallel compound beside it keeps running its own stages to
// the end, and only an operator releases the parked one.
func TestExhaustedGateParksOnlyItsOwnBranch(t *testing.T) {
	for _, gateID := range []string{"left.test.unit", "left.review"} {
		t.Run(gateID, func(t *testing.T) {
			definition := mustPrepare(t, parallelGateTemplate(1), nil)
			checkpoint, err := Initialize("run-gate-parallel", definition)
			if err != nil {
				t.Fatal(err)
			}
			// Drive the left branch to the gate under test, then reject there.
			for _, stageID := range []string{"left.plan", "left.do", "left.test.unit", "left.test.lint", "left.review"} {
				outcome := ProgramSucceeded
				if stageID == gateID {
					outcome = ProgramFailed
				}
				checkpoint = settleStage(t, checkpoint, definition, stageID, outcome)
				if stageID == gateID {
					break
				}
			}
			if checkpoint.Nodes[gateID] != NodeBlocked {
				t.Fatalf("%q = %q, want blocked", gateID, checkpoint.Nodes[gateID])
			}
			if checkpoint.Nodes["left"] != NodeRunning || checkpoint.Nodes["right"] != NodeRunning {
				t.Fatalf("compound parents = %q / %q", checkpoint.Nodes["left"], checkpoint.Nodes["right"])
			}

			// The untouched sibling runs every one of its own stages to completion.
			for _, stageID := range []string{"right.plan", "right.do", "right.test.unit", "right.test.lint", "right.review"} {
				checkpoint = settleStage(t, checkpoint, definition, stageID, ProgramSucceeded)
			}
			checkpoint, err = AdvanceUntilQuiescent(checkpoint, definition)
			if err != nil {
				t.Fatal(err)
			}
			if checkpoint.Nodes["right"] != NodeDone {
				t.Fatalf("sibling compound = %q, want done", checkpoint.Nodes["right"])
			}
			// The join is join: all and still owes the parked branch, so nothing
			// downstream activated and the run stays open on the resolution.
			if checkpoint.Nodes["join"] != NodePending || checkpoint.Status != RunRunning {
				t.Fatalf("state around the parked branch = %q / %q",
					checkpoint.Nodes["join"], checkpoint.Status)
			}
			if checkpoint.Nodes[gateID] != NodeBlocked {
				t.Fatalf("the sibling's progress disturbed the parked gate: %q", checkpoint.Nodes[gateID])
			}
		})
	}
}

// TestBlockedGateRetryRaisesTheDoCeilingAndResumesTheLoop is the deliberate
// asymmetry: retrying a parked GATE opens a window on the do anchor, not on the
// gate, and performs the same local reset a failed-but-affordable gate would.
func TestBlockedGateRetryRaisesTheDoCeilingAndResumesTheLoop(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(1), nil)
	checkpoint, err := Initialize("run-gate-retry", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramSucceeded)
	before := cloneCheckpoint(checkpoint)
	checkpoint = runStage(t, checkpoint, definition, "build.test.lint", 1, ProgramFailed)
	if checkpoint.Nodes["build.test.lint"] != NodeBlocked {
		t.Fatalf("the exhausted gate = %q", checkpoint.Nodes["build.test.lint"])
	}
	// Nothing was reset on the way to parking: the work is still done, and the
	// gate that already passed keeps its result until an operator acts.
	if checkpoint.Nodes["build.do"] != NodeDone || checkpoint.Nodes["build.test.unit"] != NodeDone {
		t.Fatalf("parking reset stages: %v", checkpoint.Nodes)
	}
	if reset, ok := definition.StageReset("build.test.lint", before, checkpoint); ok {
		t.Fatalf("parking reported a stage reset: %#v", reset)
	}

	beforeRetry := cloneCheckpoint(checkpoint)
	resolved, err := Apply(checkpoint, definition, resolve("build.test.lint", 1, ResolveRetry))
	if err != nil {
		t.Fatal(err)
	}
	// The window opened on the WORK, and the gate got none of its own.
	if got := resolved.AttemptCeilings["build.do"]; got != 2 {
		t.Fatalf("do ceiling = %d, want attempts(1) + authored budget(1)", got)
	}
	if len(resolved.AttemptCeilings) != 1 {
		t.Fatalf("attempt ceilings = %#v", resolved.AttemptCeilings)
	}
	assertNodes(t, resolved, map[string]NodeStatus{
		"start": NodeDone, "build": NodeRunning, "end": NodePending,
		"build.plan": NodeDone, "build.do": NodeReady,
		"build.test.unit": NodePending, "build.test.lint": NodePending,
		"build.review": NodePending, "build.done": NodePending,
	})
	if len(resolved.Blocked) != 0 {
		t.Fatalf("retry left the obligation behind: %#v", resolved.Blocked)
	}
	assertStageReset(t, definition, "build.test.lint", beforeRetry, resolved, StageReset{
		ParentNodeID: "build", GateNodeID: "build.test.lint", WorkNodeID: "build.do", NextWorkAttempt: 2,
	})

	// The loop resumes on the raised window and the compound finishes.
	resolved = runStage(t, resolved, definition, "build.do", 2, ProgramSucceeded)
	resolved = runStage(t, resolved, definition, "build.test.unit", 2, ProgramSucceeded)
	resolved = runStage(t, resolved, definition, "build.test.lint", 2, ProgramSucceeded)
	resolved = runStage(t, resolved, definition, "build.review", 1, ProgramSucceeded)
	resolved, _ = advanceAndPlan(t, resolved, definition)
	if resolved.Status != RunCompleted {
		t.Fatalf("run status = %q after the retried window, want completed", resolved.Status)
	}
}

// TestBlockedGateSkipPassesTheGateAndAdvancesTheStages proves skip is the
// stage-sequence equivalent of a pass: the next prepared stage activates exactly
// as a successful gate program would have left it.
func TestBlockedGateSkipPassesTheGateAndAdvancesTheStages(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(1), nil)
	checkpoint, err := Initialize("run-gate-skip", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramFailed)

	before := cloneCheckpoint(checkpoint)
	skipped, err := Apply(checkpoint, definition, resolve("build.test.unit", 1, ResolveSkip))
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Nodes["build.test.unit"] != NodeDone || skipped.Nodes["build.test.lint"] != NodeReady {
		t.Fatalf("state after skip = %v", skipped.Nodes)
	}
	// Passing a gate is not reworking, so there is no reset fact to record.
	if reset, ok := definition.StageReset("build.test.unit", before, skipped); ok {
		t.Fatalf("a skip reported a stage reset: %#v", reset)
	}
	if len(skipped.Blocked) != 0 {
		t.Fatalf("skip left the obligation behind: %#v", skipped.Blocked)
	}
	// Nothing reset: skipping is passing the gate, not re-running the work.
	if skipped.Nodes["build.do"] != NodeDone {
		t.Fatalf("skip re-ran the work: %q", skipped.Nodes["build.do"])
	}
	if got := skipped.Attempts["build.test.unit"]; got != 1 {
		t.Fatalf("skip changed the gate's attempt counter to %d", got)
	}

	skipped = runStage(t, skipped, definition, "build.test.lint", 1, ProgramSucceeded)
	skipped = runStage(t, skipped, definition, "build.review", 1, ProgramSucceeded)
	skipped, _ = advanceAndPlan(t, skipped, definition)
	if skipped.Status != RunCompleted {
		t.Fatalf("run status = %q after a skipped gate", skipped.Status)
	}
}

// TestBlockedGateCancelDrainsTheRun is the unchanged third resolution: it dooms
// the run, drops every parked obligation, and lets an in-flight sibling command
// settle honestly before the run finishes canceled.
func TestBlockedGateCancelDrainsTheRun(t *testing.T) {
	definition := mustPrepare(t, parallelGateTemplate(1), nil)
	checkpoint, err := Initialize("run-gate-cancel", definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, stageID := range []string{"left.plan", "left.do", "left.test.unit"} {
		outcome := ProgramSucceeded
		if stageID == "left.test.unit" {
			outcome = ProgramFailed
		}
		checkpoint = settleStage(t, checkpoint, definition, stageID, outcome)
	}
	checkpoint, outstanding := fillOutbox(t, checkpoint, definition)
	sibling, ok := outstanding["right.plan"]
	if !ok {
		t.Fatalf("the sibling branch has no outstanding command: %v", outstanding)
	}
	if checkpoint.Nodes["left.test.unit"] != NodeBlocked {
		t.Fatalf("left gate = %q", checkpoint.Nodes["left.test.unit"])
	}

	canceled, err := Apply(checkpoint, definition, resolve("left.test.unit", 1, ResolveCancel))
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Nodes["left.test.unit"] != NodeCanceled || len(canceled.Blocked) != 0 {
		t.Fatalf("state after cancel = %#v", canceled)
	}
	if !Draining(canceled) || canceled.Status != RunRunning {
		t.Fatalf("cancel finished the run while a sibling drained: %q", canceled.Status)
	}
	if len(canceled.Commands) != 1 || canceled.Commands[0].NodeID != "right.plan" {
		t.Fatalf("cancel disturbed the sibling outbox: %#v", canceled.Commands)
	}
	drained, err := Apply(canceled, definition, observed(&sibling, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if drained.Status != RunCanceled {
		t.Fatalf("drained run status = %q, want canceled", drained.Status)
	}
}

// TestStaleInputAroundTheReworkLoopFailsClosed covers the two boundaries the
// loop adds: an observation formed before a reset, and a resolution formed
// against a parked gate the run has already moved past.
func TestStaleInputAroundTheReworkLoopFailsClosed(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(2), nil)
	checkpoint, err := Initialize("run-gate-stale", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	checkpoint, work := advanceAndPlan(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, observed(work, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, gate := advanceAndPlan(t, checkpoint, definition)
	reset, err := Apply(checkpoint, definition, observedWithError(gate, "rejected"))
	if err != nil {
		t.Fatal(err)
	}
	before := cloneCheckpoint(reset)

	for _, test := range []struct {
		name       string
		transition Transition
	}{
		// The gate program reporting again after its verdict already reset the
		// work. Both outcomes are refused: a late success is exactly as dangerous.
		{name: "duplicate gate failure", transition: observedWithError(gate, "rejected")},
		{name: "late gate success", transition: observed(gate, ProgramSucceeded, 0)},
		// The pre-reset work attempt reporting after the reset re-readied it.
		{name: "superseded work observation", transition: observed(work, ProgramSucceeded, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Apply(reset, definition, test.transition); !errors.Is(err, ErrStaleObservation) {
				t.Fatalf("error = %v, want ErrStaleObservation", err)
			}
			if !reflect.DeepEqual(reset, before) {
				t.Fatalf("a refused observation mutated the reset state: %#v", reset)
			}
		})
	}

	// Park the gate, then measure resolutions against the exact parked identity.
	parked := runStage(t, reset, definition, "build.do", 2, ProgramSucceeded)
	parked = runStage(t, parked, definition, "build.test.unit", 2, ProgramFailed)
	if parked.Nodes["build.test.unit"] != NodeBlocked {
		t.Fatalf("gate = %q, want blocked", parked.Nodes["build.test.unit"])
	}
	parkedBefore := cloneCheckpoint(parked)
	for _, test := range []struct {
		name       string
		transition Transition
	}{
		{name: "wrong attempt", transition: resolve("build.test.unit", 1, ResolveRetry)},
		{name: "attempt ahead of the parked one", transition: resolve("build.test.unit", 3, ResolveRetry)},
		{name: "a gate that is not parked", transition: resolve("build.test.lint", 1, ResolveRetry)},
		{name: "the work stage rather than the gate", transition: resolve("build.do", 2, ResolveRetry)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Apply(parked, definition, test.transition); !errors.Is(err, ErrStaleResolution) {
				t.Fatalf("error = %v, want ErrStaleResolution", err)
			}
			if !reflect.DeepEqual(parked, parkedBefore) {
				t.Fatalf("a refused resolution mutated the checkpoint: %#v", parked)
			}
		})
	}
	// The exact identity commits, and a duplicate of it is stale afterwards.
	resolved, err := Apply(parked, definition, resolve("build.test.unit", 2, ResolveRetry))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(resolved, definition, resolve("build.test.unit", 2, ResolveRetry)); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("a duplicate resolution was accepted: %v", err)
	}
}

// TestReworkLoopSurvivesColdLoadAtEveryPosition throws the whole in-memory
// derivation away at every position of the loop — before and after each
// dispatch, across the reset, and while the gate is parked — and proves the run
// comes back identical and keeps going. Expansion, gate ceilings, and the reset
// itself are all re-derived, because none of them is persisted.
func TestReworkLoopSurvivesColdLoadAtEveryPosition(t *testing.T) {
	tmpl := gateTemplate(2)
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-gate-cold", definition)
	if err != nil {
		t.Fatal(err)
	}
	// The first pass rejects at the last gate; the second passes everywhere.
	rejected := map[string]bool{"build.review": true}
	loads := 0
	for range 8 * len(definition.nodes) {
		checkpoint, definition = coldReload(t, tmpl, checkpoint)
		loads++
		next, command, _, err := AdvanceAndPlan(checkpoint, definition)
		if err != nil {
			t.Fatalf("advance and plan: %v", err)
		}
		checkpoint = next
		if command == nil {
			break
		}
		checkpoint, definition = coldReload(t, tmpl, checkpoint)
		loads++
		transition := observed(command, ProgramSucceeded, 0)
		if rejected[command.NodeID] {
			delete(rejected, command.NodeID)
			transition = observedWithError(command, "rejected")
		}
		if checkpoint, err = Apply(checkpoint, definition, transition); err != nil {
			t.Fatalf("observe %q after cold load: %v", command.NodeID, err)
		}
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q after %d cold loads", checkpoint.Status, loads)
	}
	if got := checkpoint.Attempts["build.do"]; got != 2 {
		t.Fatalf("work attempts across the cold-loaded loop = %d, want 2", got)
	}

	// And the parked case: a blocked gate cold-loads, offers no pushable work,
	// and its resolution commits normally after the reload.
	definition = mustPrepare(t, gateTemplate(1), nil)
	parked, err := Initialize("run-gate-cold-blocked", definition)
	if err != nil {
		t.Fatal(err)
	}
	parked = runStage(t, parked, definition, "build.plan", 1, ProgramSucceeded)
	parked = runStage(t, parked, definition, "build.do", 1, ProgramSucceeded)
	parked = runStage(t, parked, definition, "build.test.unit", 1, ProgramFailed)

	encoded, err := json.Marshal(parked)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatalf("a parked gate did not cold-load: %v", err)
	}
	if !reflect.DeepEqual(loaded, parked) {
		t.Fatalf("cold load changed the parked checkpoint\n got: %#v\nwant: %#v", loaded, parked)
	}
	if Runnable(loaded, definition) {
		t.Fatal("a cold-loaded parked gate reported pushable work")
	}
	if _, err := Apply(loaded, definition, resolve("build.test.unit", 1, ResolveRetry)); err != nil {
		t.Fatalf("a resolution after cold load was refused: %v", err)
	}
}

// TestParallelCompoundsResetInIsolation is the fan-out property of the reset: a
// gate rejecting inside one compound must change nothing whatsoever outside that
// parent's own prepared child list, including in a sibling sitting at the very
// same stage.
func TestParallelCompoundsResetInIsolation(t *testing.T) {
	definition := mustPrepare(t, parallelGateTemplate(2), nil)
	checkpoint, err := Initialize("run-gate-isolation", definition)
	if err != nil {
		t.Fatal(err)
	}
	// Both compounds reach their first check, each holding its own command.
	for _, stageID := range []string{"left.plan", "right.plan", "left.do", "right.do"} {
		checkpoint = settleStage(t, checkpoint, definition, stageID, ProgramSucceeded)
	}
	checkpoint, gates := fillOutbox(t, checkpoint, definition)
	leftGate, rightGate := gates["left.test.unit"], gates["right.test.unit"]
	if leftGate.NodeID == "" || rightGate.NodeID == "" {
		t.Fatalf("both compounds should hold their own gate command: %v", gates)
	}
	before := cloneCheckpoint(checkpoint)

	reset, err := Apply(checkpoint, definition, observedWithError(&leftGate, "rejected"))
	if err != nil {
		t.Fatal(err)
	}
	// Exactly two node statuses moved, both inside the left parent's child list.
	changed := []string{}
	for nodeID, status := range reset.Nodes {
		if before.Nodes[nodeID] != status {
			changed = append(changed, nodeID)
		}
	}
	slices.Sort(changed)
	if want := []string{"left.do", "left.test.unit"}; !slices.Equal(changed, want) {
		t.Fatalf("the reset changed\n got: %v\nwant: %v", changed, want)
	}
	// The sibling's in-flight command, attempt counter, and stage are untouched.
	if sibling, ok := findCommandForTest(reset, "right.test.unit"); !ok ||
		!reflect.DeepEqual(sibling, rightGate) {
		t.Fatalf("the sibling command changed: %#v", sibling)
	}
	if !reflect.DeepEqual(reset.Attempts, before.Attempts) {
		t.Fatalf("the reset renumbered attempts\n got: %v\nwant: %v", reset.Attempts, before.Attempts)
	}
	if !reflect.DeepEqual(reset.Edges, before.Edges) {
		t.Fatalf("the reset settled an edge: %v", reset.Edges)
	}
	// Only the left gate reports a reset, and the sibling sitting at the very
	// same stage reports none.
	assertStageReset(t, definition, "left.test.unit", before, reset, StageReset{
		ParentNodeID: "left", GateNodeID: "left.test.unit", WorkNodeID: "left.do", NextWorkAttempt: 2,
	})
	if got, ok := definition.StageReset("right.test.unit", before, reset); ok {
		t.Fatalf("the sibling compound claimed a reset: %#v", got)
	}

	// Both branches then finish independently, each on its own budget.
	settled, err := Apply(reset, definition, observed(&rightGate, ProgramSucceeded, 0))
	if err != nil {
		t.Fatalf("the sibling could not settle while its neighbour reworked: %v", err)
	}
	for _, stageID := range []string{
		"left.do", "right.test.lint", "left.test.unit", "right.review",
		"left.test.lint", "left.review", "join",
	} {
		settled = settleStage(t, settled, definition, stageID, ProgramSucceeded)
	}
	settled, _ = advanceAndPlan(t, settled, definition)
	if settled.Status != RunCompleted {
		t.Fatalf("run status = %q", settled.Status)
	}
	// The reworked branch spent two work attempts; the untouched one spent one.
	if got, want := settled.Attempts["left.do"], 2; got != want {
		t.Fatalf("left work attempts = %d, want %d", got, want)
	}
	if got, want := settled.Attempts["right.do"], 1; got != want {
		t.Fatalf("right work attempts = %d, want %d", got, want)
	}
	if len(settled.AttemptCeilings) != 0 {
		t.Fatalf("the loop wrote ceilings: %#v", settled.AttemptCeilings)
	}
	if _, unit := settled.Attempts["left.test.unit"]; !unit {
		t.Fatal("the re-run gate has no attempt counter")
	}
	if got := settled.Attempts["left.test.unit"]; got != 2 {
		t.Fatalf("the re-run gate's attempts = %d, want 2", got)
	}
}

// TestGateReworkPersistsNoNewCheckpointState pins the durable encoding at the
// two states this slice creates: mid-rework and parked at an exhausted gate. The
// v3 checkpoint gains no reset cursor, no gate ceiling, no expansion record, and
// no feedback payload — a reworking compound looks exactly like ordinary nodes
// and one raised ceiling on the node that actually owns the budget.
func TestGateReworkPersistsNoNewCheckpointState(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(1), nil)
	checkpoint, err := Initialize("run-shape", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramFailed)

	parked, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	wantParked := `{"version":3,"runId":"run-shape","status":"running",` +
		`"nodes":{"build":"running","build.do":"done","build.done":"pending","build.plan":"done",` +
		`"build.review":"pending","build.test.lint":"pending","build.test.unit":"blocked",` +
		`"end":"pending","start":"done"},` +
		`"attempts":{"build.do":1,"build.plan":1,"build.test.unit":1},` +
		`"edges":{"build":{"next":"unresolved"},"start":{"next":"arrived"}},` +
		`"awaitingDecisions":null,"commands":null,` +
		`"blocked":[{"nodeId":"build.test.unit","reason":"build.test.unit rejected the work"}]}`
	if string(parked) != wantParked {
		t.Fatalf("parked checkpoint JSON\n got: %s\nwant: %s", parked, wantParked)
	}

	resolved, err := Apply(checkpoint, definition, resolve("build.test.unit", 1, ResolveRetry))
	if err != nil {
		t.Fatal(err)
	}
	reworking, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	wantReworking := `{"version":3,"runId":"run-shape","status":"running",` +
		`"nodes":{"build":"running","build.do":"ready","build.done":"pending","build.plan":"done",` +
		`"build.review":"pending","build.test.lint":"pending","build.test.unit":"pending",` +
		`"end":"pending","start":"done"},` +
		`"attempts":{"build.do":1,"build.plan":1,"build.test.unit":1},` +
		`"edges":{"build":{"next":"unresolved"},"start":{"next":"arrived"}},` +
		`"awaitingDecisions":null,"commands":null,` +
		`"attemptCeilings":{"build.do":2}}`
	if string(reworking) != wantReworking {
		t.Fatalf("reworking checkpoint JSON\n got: %s\nwant: %s", reworking, wantReworking)
	}
}

// TestGateLoadBoundaryBoundsGateAttemptsByTheirAnchor covers the two narrow
// relaxations this slice makes at the load boundary, in both directions: a gate
// attempt inside its DERIVED bound loads, one past it does not, and a parked
// gate is admitted only because its do anchor authored a retry policy.
func TestGateLoadBoundaryBoundsGateAttemptsByTheirAnchor(t *testing.T) {
	definition := mustPrepare(t, gateTemplate(2), nil)
	valid, err := Initialize("run-gate-boundary", definition)
	if err != nil {
		t.Fatal(err)
	}
	valid = runStage(t, valid, definition, "build.plan", 1, ProgramSucceeded)
	valid = runStage(t, valid, definition, "build.do", 1, ProgramSucceeded)
	valid = runStage(t, valid, definition, "build.test.unit", 1, ProgramFailed)
	valid = runStage(t, valid, definition, "build.do", 2, ProgramSucceeded)

	// A gate attempt inside the anchor's budget is exactly what the second pass
	// produces, and it loads.
	second := runStage(t, valid, definition, "build.test.unit", 2, ProgramSucceeded)
	if err := ValidateCheckpoint(second, definition); err != nil {
		t.Fatalf("a legitimately re-run gate did not load: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(Checkpoint) Checkpoint
	}{
		{name: "gate attempt past its derived ceiling", mutate: func(c Checkpoint) Checkpoint {
			c.Attempts["build.test.unit"] = 3
			return c
		}},
		{name: "gate carrying an attempt ceiling of its own", mutate: func(c Checkpoint) Checkpoint {
			c.AttemptCeilings = map[string]int{"build.test.unit": 5}
			return c
		}},
		{name: "plan stage retried past its fail-fast budget", mutate: func(c Checkpoint) Checkpoint {
			c.Attempts["build.plan"] = 2
			return c
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			broken := test.mutate(cloneCheckpoint(second))
			if err := ValidateCheckpoint(broken, definition); !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("error = %v, want an invalid checkpoint", err)
			}
		})
	}

	// A parked gate is admitted only through its anchor's authored policy: the
	// same shape under a compound with no retry at all is refused.
	parked := runStage(t, valid, definition, "build.test.unit", 2, ProgramFailed)
	if err := ValidateCheckpoint(parked, definition); err != nil {
		t.Fatalf("a parked gate did not load: %v", err)
	}
	failFast := mustPrepare(t, gateTemplate(0), nil)
	forged, err := Initialize("run-gate-boundary", failFast)
	if err != nil {
		t.Fatal(err)
	}
	forged.Nodes["build.test.unit"] = NodeBlocked
	forged.Attempts = map[string]int{"build.test.unit": 1}
	forged.Blocked = []BlockedObligation{{NodeID: "build.test.unit"}}
	if err := ValidateCheckpoint(forged, failFast); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("a gate parked under a compound with no authored retry loaded: %v", err)
	}
}

// wideParallelGateTemplate forks into `width` identical retryable compounds.
func wideParallelGateTemplate(width int) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
		"join":  joinAllTask("end", "true"),
		"end":   {Type: model.NodeTypeEnd},
	}
	next := model.Next{}
	for i := range width {
		id := fmt.Sprintf("c%03d", i)
		next[id] = id
		node := compoundTask("join")
		node.Retry = &model.RetryPolicy{MaxAttempts: 2}
		nodes[id] = node
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: next}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "wide-gate", Start: "start", Nodes: nodes,
	}
}

// BenchmarkCompoundStageReset is the complexity evidence for the reset itself:
// it is linear in ONE parent's prepared children and independent of how many
// other compounds the run has, which is what "the reset never scans another
// parent" means in practice rather than as an assertion.
func BenchmarkCompoundStageReset(b *testing.B) {
	for _, width := range []int{2, 8, 64, 256} {
		b.Run(fmt.Sprintf("compounds=%d", width), func(b *testing.B) {
			definition, err := Prepare(wideParallelGateTemplate(width), nil)
			if err != nil {
				b.Fatal(err)
			}
			checkpoint, err := Initialize("run-gate-bench", definition)
			if err != nil {
				b.Fatal(err)
			}
			// Put the first compound in the state a rejected gate arrives at: the
			// parent running, the plan and the work done, the gate under way.
			checkpoint.Nodes["c000"] = NodeRunning
			checkpoint.Nodes["c000.plan"] = NodeDone
			gateIndex := definition.index["c000.test.unit"]
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				// Restoring the two statuses the reset moves is O(1), so the
				// measurement stays on the reset instead of on cloning a checkpoint
				// that grows with the number of OTHER compounds.
				checkpoint.Nodes["c000.do"] = NodeDone
				checkpoint.Nodes["c000.test.unit"] = NodeRunning
				if err := resetCompoundStages(&checkpoint, definition, gateIndex); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStageReset measures the evidence lookup a commit that reworked
// nothing pays — the overwhelmingly common case, and the one that used to be a
// walk of every compound in the run. The named node is an ordinary program task
// with no compound anywhere near it, so this is the cost of the whole derivation
// on an unrelated transition, at growing run sizes.
func BenchmarkStageReset(b *testing.B) {
	for _, width := range []int{2, 8, 256} {
		b.Run(fmt.Sprintf("compounds=%d", width), func(b *testing.B) {
			definition, err := Prepare(wideParallelGateTemplate(width), nil)
			if err != nil {
				b.Fatal(err)
			}
			before, err := Initialize("run-gate-evidence-bench", definition)
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
				if _, ok := definition.StageReset("join", before, after); ok {
					b.Fatal("an unrelated node reported a stage reset")
				}
			}
		})
	}
}
