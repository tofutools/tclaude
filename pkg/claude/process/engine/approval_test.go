package engine

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// approvalTemplate is the human-approval fixture: start -> build -> end, where
// build's plan stage declares approval: human on top of the canonical compound.
// The check and review stages stay, so the gate has real neighbours and "an
// approval never reopens for a later gate" is measurable rather than asserted.
//
// maxAttempts of zero authors no retry policy at all, which is the fail-fast
// compound the recurring-approval tests measure the retryable one against.
func approvalTemplate(maxAttempts int) *model.Template {
	return compoundStageTemplate(func(node *model.Node) {
		node.Plan.Approval = model.PlanApprovalHuman
		if maxAttempts > 0 {
			node.Retry = &model.RetryPolicy{MaxAttempts: maxAttempts}
		}
	})
}

// approvalStageIDs is the fixture's exact expansion, in order. The gate sits
// between plan and do, which is the whole point of approving a plan.
var approvalStageIDs = []string{
	"build.plan", "build.plan.approval", "build.do", "build.test.unit", "build.review", "build.done",
}

// decidedAt is the decision transition with its window named. The existing
// decided() helper deliberately keeps sending no attempt at all, which is what
// keeps every authored-decision test a live regression on the zero default.
func decidedAt(nodeID, verdict string, attempt int) Transition {
	return Transition{Kind: TransitionDecisionRecorded,
		Decision: &DecisionRecord{NodeID: nodeID, Verdict: verdict, Attempt: attempt}}
}

// awaitApproval drives a fresh run up to its first open approval window.
func awaitApproval(t *testing.T, runID string, definition *Definition) Checkpoint {
	t.Helper()
	checkpoint, err := Initialize(runID, definition)
	if err != nil {
		t.Fatal(err)
	}
	return runStage(t, checkpoint, definition, "build.plan", 1, ProgramSucceeded)
}

// decide applies one verdict and fails the test if the engine refused it.
func decide(t *testing.T, checkpoint Checkpoint, definition *Definition,
	nodeID, verdict string, attempt int) Checkpoint {
	t.Helper()
	next, err := Apply(checkpoint, definition, decidedAt(nodeID, verdict, attempt))
	if err != nil {
		t.Fatalf("decide %q %q attempt %d: %v", nodeID, verdict, attempt, err)
	}
	return next
}

// assertRefusedWithoutChange applies a transition that must be refused and
// proves the checkpoint is untouched by comparing its durable encoding, not
// just the fields the test happened to think of.
func assertRefusedWithoutChange(t *testing.T, checkpoint Checkpoint, definition *Definition,
	transition Transition, want error) {
	t.Helper()
	before, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(checkpoint, definition, transition); !errors.Is(err, want) {
		t.Fatalf("apply %#v error = %v, want %v", transition.Decision, err, want)
	}
	after, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("a refused transition mutated the caller's state\n got: %s\nwant: %s", after, before)
	}
}

func TestPrepareExpandsHumanPlanApprovalIntoAFixedVerdictDecisionStage(t *testing.T) {
	tmpl := approvalTemplate(0)
	assertAuthoringValid(t, tmpl)
	if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
		t.Fatalf("a human plan approval gate was refused: %#v", diagnostics)
	}
	definition := mustPrepare(t, tmpl, nil)

	parent := definition.nodes[definition.index["build"]]
	stageIDs := make([]string, 0, len(parent.children))
	for _, childIndex := range parent.children {
		stageIDs = append(stageIDs, definition.nodes[childIndex].id)
	}
	if !slices.Equal(stageIDs, approvalStageIDs) {
		t.Fatalf("derived stages\n got: %v\nwant: %v", stageIDs, approvalStageIDs)
	}

	// The gate is an ordinary prepared decision with a FIXED sorted vocabulary,
	// and it is never bound to a program: model.ExpandNode gives it a synthetic
	// human performer describing who decides, not something to run.
	gate := definition.nodes[definition.index["build.plan.approval"]]
	if gate.kind != definitionDecision {
		t.Fatalf("approval gate kind = %d, want a prepared decision", gate.kind)
	}
	if want := []string{ApprovalApprove, ApprovalRework}; !slices.Equal(gate.verdicts, want) {
		t.Fatalf("approval verdicts\n got: %v\nwant: %v", gate.verdicts, want)
	}
	if !reflect.DeepEqual(gate.program, ProgramCommand{}) {
		t.Fatalf("the approval gate was bound to a program: %#v", gate.program)
	}
	if verdicts, ok := definition.DecisionVerdicts("build.plan.approval"); !ok ||
		!slices.Equal(verdicts, []string{ApprovalApprove, ApprovalRework}) {
		t.Fatalf("public verdicts = %v (ok=%v)", verdicts, ok)
	}
	// A stage carries no authored edges, so the gate has none to settle and no
	// verdict of it selects one.
	if len(gate.outgoing) != 0 || len(gate.incoming) != 0 {
		t.Fatalf("the approval gate was prepared with edges: %d out, %d in", len(gate.outgoing), len(gate.incoming))
	}
	for _, verdict := range []string{ApprovalApprove, ApprovalRework} {
		if edge, ok := definition.DecisionEdge("build.plan.approval", verdict); ok {
			t.Fatalf("verdict %q invented an authored edge: %#v", verdict, edge)
		}
	}

	// Both anchors of the pair are prepared, so neither "which work is this gate
	// about" nor "is this plan gated by a person" is ever a walk.
	if got := definition.nodes[parent.planAnchor].id; got != "build.plan" {
		t.Fatalf("prepared plan anchor = %q", got)
	}
	if got := definition.nodes[parent.approvalGate].id; got != "build.plan.approval" {
		t.Fatalf("prepared approval gate = %q", got)
	}
	if anchor, ok := approvalAnchor(definition, gate); !ok || anchor.id != "build.plan" {
		t.Fatalf("approval anchor = %q (ok=%v)", anchor.id, ok)
	}
	// TCL-728's gate semantics are untouched: an approval is not a do-budget gate
	// and can never park on an operator.
	if anchor, ok := gateAnchor(definition, gate); ok {
		t.Fatalf("the approval gate claimed a do anchor: %q", anchor.id)
	}
	if blockableTask(definition, gate) {
		t.Fatalf("the approval gate reported itself blockable")
	}

	// A compound without the authored approval prepares no gate and no anchor,
	// and its plan stage keeps its plain fail-fast ceiling.
	plain := mustPrepare(t, compoundStageTemplate(nil), nil)
	if _, ok := plain.index["build.plan.approval"]; ok {
		t.Fatalf("an unapproved plan expanded a gate")
	}
	if got := plain.nodes[plain.index["build"]].approvalGate; got != -1 {
		t.Fatalf("unapproved compound approval gate anchor = %d, want -1", got)
	}
	empty, err := Initialize("run-approval-shape", plain)
	if err != nil {
		t.Fatal(err)
	}
	if got := executableAttemptCeiling(empty, plain, plain.nodes[plain.index["build.plan"]]); got != 1 {
		t.Fatalf("unapproved plan ceiling = %d, want its authored 1", got)
	}
}

// TestApprovalRetryStaysIneligible pins the one plan-approval axis this slice
// deliberately does NOT execute. A rework budget is not what bounds a human
// gate: each rework is already one explicit audited action.
func TestApprovalRetryStaysIneligible(t *testing.T) {
	tmpl := approvalTemplate(0)
	build := tmpl.Nodes["build"]
	build.Plan.ApprovalRetry = &model.RetryPolicy{MaxAttempts: 2}
	tmpl.Nodes["build"] = build
	assertAuthoringValid(t, tmpl)
	if !hasCodeAtPath(CheckEligibility(tmpl), "unsupported_retry", "nodes.build.plan.approvalRetry") {
		t.Fatalf("approvalRetry became eligible: %#v", CheckEligibility(tmpl))
	}
}

// TestPlanSuccessOpensTheApprovalWindowAndApproveReadiesDo is the happy path:
// the window opens the moment the plan succeeds, holds the run on a human
// without stopping anything else, and approve steps the compound to its do
// stage exactly as a successful stage would have.
func TestPlanSuccessOpensTheApprovalWindowAndApproveReadiesDo(t *testing.T) {
	definition := mustPrepare(t, approvalTemplate(0), nil)
	checkpoint := awaitApproval(t, "run-approval-approve", definition)

	// The window is open, durable, and identified by the plan attempt it is about.
	assertNodes(t, checkpoint, map[string]NodeStatus{
		"start": NodeDone, "build": NodeRunning, "end": NodePending,
		"build.plan": NodeDone, "build.plan.approval": NodeReady, "build.do": NodePending,
		"build.test.unit": NodePending, "build.review": NodePending, "build.done": NodePending,
	})
	if want := []DecisionObligation{{NodeID: "build.plan.approval"}}; !reflect.DeepEqual(checkpoint.AwaitingDecisions, want) {
		t.Fatalf("awaiting decisions\n got: %v\nwant: %v", checkpoint.AwaitingDecisions, want)
	}
	if attempt, ok := definition.ApprovalAttempt(checkpoint, "build.plan.approval"); !ok || attempt != 1 {
		t.Fatalf("approval window = %d (ok=%v), want plan attempt 1", attempt, ok)
	}
	if got := definition.RequiredDecisionAttempt(checkpoint, "build.plan.approval"); got != 1 {
		t.Fatalf("required attempt = %d, want 1", got)
	}
	// Nothing engine-owned or plannable is left: the run genuinely waits on a
	// person rather than quietly running the do stage past an unapproved plan.
	if Runnable(checkpoint, definition) {
		t.Fatalf("a run awaiting approval reported plannable work")
	}

	checkpoint = decide(t, checkpoint, definition, "build.plan.approval", ApprovalApprove, 1)
	assertNodes(t, checkpoint, map[string]NodeStatus{
		"start": NodeDone, "build": NodeRunning, "end": NodePending,
		"build.plan": NodeDone, "build.plan.approval": NodeDone, "build.do": NodeReady,
		"build.test.unit": NodePending, "build.review": NodePending, "build.done": NodePending,
	})
	if len(checkpoint.AwaitingDecisions) != 0 {
		t.Fatalf("the resolved obligation survived: %v", checkpoint.AwaitingDecisions)
	}
	// Approving settles nothing durable of its own: no edge, no counter, no
	// ceiling. The compound's authored route still belongs to its done stage.
	if checkpoint.Edges["build"][model.DefaultOutcome] != EdgeUnresolved {
		t.Fatalf("approval settled the parent's authored route: %q",
			checkpoint.Edges["build"][model.DefaultOutcome])
	}
	if len(checkpoint.AttemptCeilings) != 0 {
		t.Fatalf("approval wrote an attempt ceiling: %v", checkpoint.AttemptCeilings)
	}
	if want := (map[string]int{"build.plan": 1}); !reflect.DeepEqual(checkpoint.Attempts, want) {
		t.Fatalf("attempts after approval\n got: %v\nwant: %v", checkpoint.Attempts, want)
	}

	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.review", 1, ProgramSucceeded)
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command != nil {
		t.Fatalf("planned past the finished compound: %#v", command)
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
	if got := checkpoint.Attempts["build.plan"]; got != 1 {
		t.Fatalf("an approved plan ran %d times, want 1", got)
	}
}

// TestTwoReworksRunThePlanThreeTimes is the recurring loop measured directly:
// each explicit human rework buys exactly one more plan execution, attempts stay
// monotonic, and nothing durable accumulates.
func TestTwoReworksRunThePlanThreeTimes(t *testing.T) {
	definition := mustPrepare(t, approvalTemplate(0), nil)
	checkpoint := awaitApproval(t, "run-approval-rework", definition)

	for attempt := 1; attempt <= 2; attempt++ {
		before := cloneCheckpoint(checkpoint)
		checkpoint = decide(t, checkpoint, definition, "build.plan.approval", ApprovalRework, attempt)

		// Exactly two nodes move, and every stage after the gate — which has never
		// run — is left alone rather than swept.
		assertNodes(t, checkpoint, map[string]NodeStatus{
			"start": NodeDone, "build": NodeRunning, "end": NodePending,
			"build.plan": NodeReady, "build.plan.approval": NodePending, "build.do": NodePending,
			"build.test.unit": NodePending, "build.review": NodePending, "build.done": NodePending,
		})
		if len(checkpoint.AwaitingDecisions) != 0 || len(checkpoint.Commands) != 0 ||
			len(checkpoint.Blocked) != 0 || len(checkpoint.AttemptCeilings) != 0 {
			t.Fatalf("rework wrote durable state of its own: %#v", checkpoint)
		}
		if !reflect.DeepEqual(checkpoint.Attempts, before.Attempts) {
			t.Fatalf("rework moved an attempt counter\n got: %v\nwant: %v", checkpoint.Attempts, before.Attempts)
		}
		if checkpoint.Edges["build"][model.DefaultOutcome] != EdgeUnresolved {
			t.Fatalf("rework settled the parent's authored route")
		}
		// One compact reset fact is derivable for evidence, naming the PLAN as the
		// work that runs again and the attempt it will carry.
		assertStageReset(t, definition, "build.plan.approval", before, checkpoint, StageReset{
			ParentNodeID: "build", GateNodeID: "build.plan.approval",
			WorkNodeID: "build.plan", NextWorkAttempt: attempt + 1,
		})
		for _, nodeID := range []string{
			"build", "build.plan", "build.do", "build.test.unit", "build.review", "build.done",
			"start", "end", "nowhere",
		} {
			if reset, ok := definition.StageReset(nodeID, before, checkpoint); ok {
				t.Fatalf("node %q claimed the approval reset: %#v", nodeID, reset)
			}
		}

		// Ordinary planning mints the next plan attempt, with no ceiling entry to
		// make room for it.
		checkpoint = runStage(t, checkpoint, definition, "build.plan", attempt+1, ProgramSucceeded)
		if got := definition.RequiredDecisionAttempt(checkpoint, "build.plan.approval"); got != attempt+1 {
			t.Fatalf("reopened window = %d, want %d", got, attempt+1)
		}
	}

	if got := checkpoint.Attempts["build.plan"]; got != 3 {
		t.Fatalf("plan attempts after two reworks = %d, want 3", got)
	}
	checkpoint = decide(t, checkpoint, definition, "build.plan.approval", ApprovalApprove, 3)
	if checkpoint.Nodes["build.do"] != NodeReady {
		t.Fatalf("do stage after the third plan was approved = %q", checkpoint.Nodes["build.do"])
	}
	if len(checkpoint.AttemptCeilings) != 0 {
		t.Fatalf("two reworks wrote %v into attemptCeilings", checkpoint.AttemptCeilings)
	}
}

// TestApprovalVerdictIsBoundToItsExactWindow is the stale-safety proof. A
// recurring window is only decidable by the exact input the run is asking for,
// and every refusal leaves the durable encoding byte-identical.
func TestApprovalVerdictIsBoundToItsExactWindow(t *testing.T) {
	definition := mustPrepare(t, approvalTemplate(0), nil)
	pending := awaitApproval(t, "run-approval-stale", definition)

	// While window 1 is open: a verdict for any other window, node, or verdict is
	// refused, and so is one that names no window at all.
	for name, transition := range map[string]Transition{
		"next window":     decidedAt("build.plan.approval", ApprovalApprove, 2),
		"no window":       decidedAt("build.plan.approval", ApprovalApprove, 0),
		"earlier window":  decidedAt("build.plan.approval", ApprovalRework, -1),
		"legacy caller":   decided("build.plan.approval", ApprovalApprove),
		"wrong node":      decidedAt("build.plan", ApprovalApprove, 1),
		"unrelated node":  decidedAt("build.do", ApprovalApprove, 1),
		"unknown node":    decidedAt("nowhere", ApprovalApprove, 1),
		"authored labels": decidedAt("build.plan.approval", model.DefaultOutcome, 1),
	} {
		want := ErrStaleDecision
		if name == "authored labels" {
			want = ErrInvalidDecisionVerdict
		}
		t.Run(name, func(t *testing.T) {
			assertRefusedWithoutChange(t, pending, definition, transition, want)
		})
	}

	// The window is decided once. A duplicate of the verdict that just resolved
	// it is refused as staleness, not applied twice.
	reworked := decide(t, pending, definition, "build.plan.approval", ApprovalRework, 1)
	assertRefusedWithoutChange(t, reworked, definition,
		decidedAt("build.plan.approval", ApprovalRework, 1), ErrStaleDecision)
	assertRefusedWithoutChange(t, reworked, definition,
		decidedAt("build.plan.approval", ApprovalApprove, 1), ErrStaleDecision)

	// And once window 2 has opened, the verdict a human formed against window 1
	// while it was closed is still refused — this is the delayed-input case the
	// derived identity exists for.
	reopened := runStage(t, reworked, definition, "build.plan", 2, ProgramSucceeded)
	assertRefusedWithoutChange(t, reopened, definition,
		decidedAt("build.plan.approval", ApprovalApprove, 1), ErrStaleDecision)
	assertRefusedWithoutChange(t, reopened, definition,
		decidedAt("build.plan.approval", ApprovalRework, 1), ErrStaleDecision)
	if _, err := Apply(reopened, definition, decidedAt("build.plan.approval", ApprovalApprove, 2)); err != nil {
		t.Fatalf("the current window refused its own verdict: %v", err)
	}
}

// TestAuthoredDecisionsStayOnWindowZero is the compatibility half: an authored
// decision opens exactly once, so callers that never learned about the field
// keep working and a caller that invents a window is refused.
func TestAuthoredDecisionsStayOnWindowZero(t *testing.T) {
	definition := mustPrepare(t, decisionMultiEndTemplate(), nil)
	checkpoint := advanceToDecision(t, "run-approval-authored", definition, "choose")

	if _, ok := definition.ApprovalAttempt(checkpoint, "choose"); ok {
		t.Fatalf("an authored decision reported an approval window")
	}
	if got := definition.RequiredDecisionAttempt(checkpoint, "choose"); got != 0 {
		t.Fatalf("authored required attempt = %d, want 0", got)
	}
	assertRefusedWithoutChange(t, checkpoint, definition, decidedAt("choose", "proceed", 1), ErrStaleDecision)

	// Both the field-less caller and an explicit zero decide it, and the authored
	// edge still settles exactly as it always did.
	settled, err := Apply(checkpoint, definition, decided("choose", "proceed"))
	if err != nil {
		t.Fatalf("an authored decision refused a caller that sent no window: %v", err)
	}
	explicit, err := Apply(checkpoint, definition, decidedAt("choose", "proceed", 0))
	if err != nil {
		t.Fatal(err)
	}
	left, err := json.Marshal(settled)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Fatalf("an explicit zero window differs from an absent one\n got: %s\nwant: %s", right, left)
	}
	if settled.Edges["choose"]["proceed"] != EdgeArrived || settled.Edges["choose"]["abort"] != EdgeNotTaken {
		t.Fatalf("authored decision edges\n got: %v", settled.Edges["choose"])
	}
}

// TestPlanProgramFailureAfterReworkStaysFailFast pins what a rework does NOT
// buy. A human rework buys another plan RUN; it does not turn the plan stage
// into a retryable one, and the authored ceiling is still what the failure
// disposition reads.
func TestPlanProgramFailureAfterReworkStaysFailFast(t *testing.T) {
	// The compound authors a retry budget of three, which belongs to its do stage
	// and must not leak into the plan stage a human keeps reopening.
	definition := mustPrepare(t, approvalTemplate(3), nil)
	checkpoint := awaitApproval(t, "run-approval-failfast", definition)
	checkpoint = decide(t, checkpoint, definition, "build.plan.approval", ApprovalRework, 1)
	checkpoint = runStage(t, checkpoint, definition, "build.plan", 2, ProgramFailed)

	if checkpoint.Nodes["build.plan"] != NodeFailed {
		t.Fatalf("plan stage after a failed program = %q, want failed", checkpoint.Nodes["build.plan"])
	}
	if len(checkpoint.Blocked) != 0 {
		t.Fatalf("a failed plan parked a branch: %#v", checkpoint.Blocked)
	}
	if checkpoint.Status != RunFailed {
		t.Fatalf("run status = %q, want failed", checkpoint.Status)
	}
	// A doomed run drops the human outbox, so no approval is left to offer.
	if len(checkpoint.AwaitingDecisions) != 0 {
		t.Fatalf("a doomed run kept an approval obligation: %v", checkpoint.AwaitingDecisions)
	}
}

// TestCheckReworkNeverReopensPlanApproval is the boundary between this slice
// and TCL-728: a program gate's reset starts at the do stage, so an approved
// plan stays approved however many times the work is sent back.
func TestCheckReworkNeverReopensPlanApproval(t *testing.T) {
	definition := mustPrepare(t, approvalTemplate(2), nil)
	checkpoint := awaitApproval(t, "run-approval-check-rework", definition)
	checkpoint = decide(t, checkpoint, definition, "build.plan.approval", ApprovalApprove, 1)
	checkpoint = runStage(t, checkpoint, definition, "build.do", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 1, ProgramFailed)

	assertNodes(t, checkpoint, map[string]NodeStatus{
		"start": NodeDone, "build": NodeRunning, "end": NodePending,
		"build.plan": NodeDone, "build.plan.approval": NodeDone, "build.do": NodeReady,
		"build.test.unit": NodePending, "build.review": NodePending, "build.done": NodePending,
	})
	if len(checkpoint.AwaitingDecisions) != 0 {
		t.Fatalf("a check rework reopened the plan approval: %v", checkpoint.AwaitingDecisions)
	}
	if got := checkpoint.Attempts["build.plan"]; got != 1 {
		t.Fatalf("a check rework re-ran the plan: %d attempts", got)
	}

	// The reset it DID cause is still the do-anchored one, named by its own gate.
	checkpoint = runStage(t, checkpoint, definition, "build.do", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.test.unit", 2, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "build.review", 1, ProgramSucceeded)
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
}

// TestParallelCompoundApprovalsStayIsolated proves the reset really is bounded
// by one parent: two compounds awaiting approval side by side each decide their
// own window, and neither verdict touches the other branch.
func TestParallelCompoundApprovalsStayIsolated(t *testing.T) {
	left := compoundTask("join")
	left.Plan.Approval = model.PlanApprovalHuman
	right := compoundTask("join")
	right.Plan.Approval = model.PlanApprovalHuman
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "approval-parallel", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork": {Type: model.NodeTypeParallel,
				Next: model.Next{"left": "left", "right": "right"}},
			"left":  left,
			"right": right,
			"join":  {Type: model.NodeTypeTask, Join: model.JoinAll, Next: model.Next{model.DefaultOutcome: "end"}, Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "worker", Run: "join"}},
			"end":   {Type: model.NodeTypeEnd},
		},
	}
	assertAuthoringValid(t, tmpl)
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-approval-parallel", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = runStage(t, checkpoint, definition, "left.plan", 1, ProgramSucceeded)
	checkpoint = runStage(t, checkpoint, definition, "right.plan", 1, ProgramSucceeded)

	// Both windows are open at once, each addressable on its own.
	if want := []DecisionObligation{{NodeID: "left.plan.approval"}, {NodeID: "right.plan.approval"}}; !reflect.DeepEqual(checkpoint.AwaitingDecisions, want) {
		t.Fatalf("awaiting decisions\n got: %v\nwant: %v", checkpoint.AwaitingDecisions, want)
	}

	// Reworking the left branch leaves the right branch's window and plan result
	// exactly where they were.
	checkpoint = decide(t, checkpoint, definition, "left.plan.approval", ApprovalRework, 1)
	if checkpoint.Nodes["right.plan"] != NodeDone || checkpoint.Nodes["right.plan.approval"] != NodeReady {
		t.Fatalf("the left rework moved the right branch: plan %q, approval %q",
			checkpoint.Nodes["right.plan"], checkpoint.Nodes["right.plan.approval"])
	}
	if want := []DecisionObligation{{NodeID: "right.plan.approval"}}; !reflect.DeepEqual(checkpoint.AwaitingDecisions, want) {
		t.Fatalf("obligations after one rework\n got: %v\nwant: %v", checkpoint.AwaitingDecisions, want)
	}

	// Approving the right branch while the left one is re-planning is likewise
	// local, and the two windows never share an identity.
	checkpoint = decide(t, checkpoint, definition, "right.plan.approval", ApprovalApprove, 1)
	if checkpoint.Nodes["right.do"] != NodeReady || checkpoint.Nodes["left.plan"] != NodeReady {
		t.Fatalf("branches after the right approval: right.do %q, left.plan %q",
			checkpoint.Nodes["right.do"], checkpoint.Nodes["left.plan"])
	}
	checkpoint = runStage(t, checkpoint, definition, "left.plan", 2, ProgramSucceeded)
	if got := definition.RequiredDecisionAttempt(checkpoint, "left.plan.approval"); got != 2 {
		t.Fatalf("left window = %d, want 2", got)
	}
	// The right branch's gate is already done and derives its plan attempt, not
	// the left branch's.
	if got := definition.RequiredDecisionAttempt(checkpoint, "right.plan.approval"); got != 1 {
		t.Fatalf("right window = %d, want 1", got)
	}
}

// TestApprovalCheckpointStaysExactlyV3 is the durable-shape guard: an awaited
// approval and a mid-rework state are both encoded entirely by fields v3 already
// had, and a cold load reconstructs them.
func TestApprovalCheckpointStaysExactlyV3(t *testing.T) {
	tmpl := approvalTemplate(0)
	definition := mustPrepare(t, tmpl, nil)
	awaiting := awaitApproval(t, "run-approval-v3", definition)

	encoded, err := json.Marshal(awaiting)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":3,"runId":"run-approval-v3","status":"running",` +
		`"nodes":{"build":"running","build.do":"pending","build.done":"pending","build.plan":"done",` +
		`"build.plan.approval":"ready","build.review":"pending","build.test.unit":"pending",` +
		`"end":"pending","start":"done"},` +
		`"attempts":{"build.plan":1},` +
		`"edges":{"build":{"next":"unresolved"},"start":{"next":"arrived"}},` +
		`"awaitingDecisions":[{"nodeId":"build.plan.approval"}],` +
		`"commands":null}`
	if string(encoded) != want {
		t.Fatalf("awaiting-approval checkpoint\n got: %s\nwant: %s", encoded, want)
	}

	// A restart while the window is open reconstructs it from the same bytes: the
	// window identity is re-derived from the plan counter, so the run is still
	// asking the same question.
	reloaded, reprepared := coldReload(t, tmpl, awaiting)
	if got := reprepared.RequiredDecisionAttempt(reloaded, "build.plan.approval"); got != 1 {
		t.Fatalf("window after restart = %d, want 1", got)
	}
	resumed := decide(t, reloaded, reprepared, "build.plan.approval", ApprovalRework, 1)

	// Mid-rework: the plan is ready again, the gate is pending, and nothing new
	// appears in the encoding.
	midRework, err := json.Marshal(resumed)
	if err != nil {
		t.Fatal(err)
	}
	wantMid := `{"version":3,"runId":"run-approval-v3","status":"running",` +
		`"nodes":{"build":"running","build.do":"pending","build.done":"pending","build.plan":"ready",` +
		`"build.plan.approval":"pending","build.review":"pending","build.test.unit":"pending",` +
		`"end":"pending","start":"done"},` +
		`"attempts":{"build.plan":1},` +
		`"edges":{"build":{"next":"unresolved"},"start":{"next":"arrived"}},` +
		`"awaitingDecisions":null,` +
		`"commands":null}`
	if string(midRework) != wantMid {
		t.Fatalf("mid-rework checkpoint\n got: %s\nwant: %s", midRework, wantMid)
	}

	// And a restart mid-rework picks the re-planning up where it was left.
	reloaded, reprepared = coldReload(t, tmpl, resumed)
	reloaded = runStage(t, reloaded, reprepared, "build.plan", 2, ProgramSucceeded)
	if got := reprepared.RequiredDecisionAttempt(reloaded, "build.plan.approval"); got != 2 {
		t.Fatalf("window after a mid-rework restart = %d, want 2", got)
	}
}

// TestApprovalObligationRequiresARecordedPlanAttempt is the fail-CLOSED proof
// for a window that has no identity.
//
// The reducer can never produce this state — a gate is readied only by a plan
// observation, which implies a planned attempt — so it is reachable only as
// malformed durable state. It matters anyway because the failure would be OPEN
// rather than closed: the derived window would read as zero, and zero is exactly
// what an authored decision requires, so a verdict naming no window at all would
// decide a gate the run never opened. Reworking on that verdict would then mint
// plan attempt 1 a second time, reusing the deterministic identity of work the
// same checkpoint says already completed.
func TestApprovalObligationRequiresARecordedPlanAttempt(t *testing.T) {
	tmpl := approvalTemplate(0)
	definition := mustPrepare(t, tmpl, nil)
	awaiting := awaitApproval(t, "run-approval-no-attempt", definition)

	// The one thing that gives the window its identity is removed, and nothing
	// else about the state is touched: the plan is still done and the gate is
	// still ready and obligated.
	broken := cloneCheckpoint(awaiting)
	delete(broken.Attempts, "build.plan")
	broken.Attempts = nil

	if err := ValidateCheckpoint(broken, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("validate error = %v, want an invalid checkpoint", err)
	}
	encoded, err := json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(encoded, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("decode error = %v, want an invalid checkpoint", err)
	}

	// And the reducer refuses the same identity at the point it is consumed, so
	// a caller that reached this state another way fails closed too. Both
	// verdicts are refused, and neither the no-window nor the explicit-zero form
	// gets through.
	for _, transition := range []Transition{
		decided("build.plan.approval", ApprovalApprove),
		decidedAt("build.plan.approval", ApprovalApprove, 0),
		decidedAt("build.plan.approval", ApprovalRework, 0),
		decidedAt("build.plan.approval", ApprovalRework, 1),
	} {
		assertRefusedWithoutChange(t, broken, definition, transition, ErrInvalidTransition)
	}

	// A zero counter is the same absence written out longhand, and is refused
	// identically rather than treated as "attempt zero".
	zeroed := cloneCheckpoint(awaiting)
	zeroed.Attempts["build.plan"] = 0
	if err := ValidateCheckpoint(zeroed, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("zero-counter validate error = %v, want an invalid checkpoint", err)
	}

	// The guard is scoped to approval gates: an authored decision genuinely has
	// no attempt counter and must keep loading and deciding on window zero.
	authored := mustPrepare(t, decisionMultiEndTemplate(), nil)
	choosing := advanceToDecision(t, "run-approval-authored-load", authored, "choose")
	if err := ValidateCheckpoint(choosing, authored); err != nil {
		t.Fatalf("an authored decision was refused for having no counter: %v", err)
	}
	if _, err := Apply(choosing, authored, decided("choose", "proceed")); err != nil {
		t.Fatalf("an authored decision refused its own verdict: %v", err)
	}
}

// TestApprovalWindowSaturatesRatherThanWrapping is arithmetic correctness at the
// last representable window, not a budget.
//
// The counter cannot reach this by execution — it would take 2^63 planned
// attempts — so this is about a checkpoint that CLAIMS to be there being refused
// safely rather than wrapping into a negative attempt every budget comparison
// would wave through.
func TestApprovalWindowSaturatesRatherThanWrapping(t *testing.T) {
	tmpl := approvalTemplate(0)
	definition := mustPrepare(t, tmpl, nil)
	awaiting := awaitApproval(t, "run-approval-overflow", definition)

	last := cloneCheckpoint(awaiting)
	last.Attempts["build.plan"] = math.MaxInt

	// The derived ceiling saturates, so the load boundary still accepts the
	// counter it would otherwise have rejected against a wrapped negative bound.
	plan := definition.nodes[definition.index["build.plan"]]
	if got := executableAttemptCeiling(last, definition, plan); got != math.MaxInt {
		t.Fatalf("saturated ceiling = %d, want %d", got, math.MaxInt)
	}
	reloaded, reprepared := coldReload(t, tmpl, last)
	if got := reprepared.RequiredDecisionAttempt(reloaded, "build.plan.approval"); got != math.MaxInt {
		t.Fatalf("window after reload = %d, want %d", got, math.MaxInt)
	}

	// Rework is refused before anything moves: readying the plan would leave the
	// branch holding work whose next attempt cannot be minted.
	assertRefusedWithoutChange(t, reloaded, reprepared,
		decidedAt("build.plan.approval", ApprovalRework, math.MaxInt), ErrInvalidTransition)

	// Approving it is still allowed — it asks for no further plan run — and the
	// compound goes on to its do stage exactly as at any other window.
	approved := decide(t, reloaded, reprepared, "build.plan.approval", ApprovalApprove, math.MaxInt)
	if approved.Nodes["build.do"] != NodeReady || approved.Nodes["build.plan.approval"] != NodeDone {
		t.Fatalf("approving the last window: do %q, gate %q",
			approved.Nodes["build.do"], approved.Nodes["build.plan.approval"])
	}
	if got := approved.Attempts["build.plan"]; got != math.MaxInt {
		t.Fatalf("approval moved the counter to %d", got)
	}

	// Planning refuses a wrapped attempt rather than minting one, wherever the
	// counter came from. This is the single mint point, so the guard covers every
	// node kind rather than only this one.
	wedged := cloneCheckpoint(reloaded)
	wedged.Nodes["build.plan"] = NodeReady
	wedged.Nodes["build.plan.approval"] = NodePending
	wedged.AwaitingDecisions = nil
	if _, _, _, err := AdvanceAndPlan(wedged, reprepared); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("planning past the last attempt: %v", err)
	}

	// Evidence saturates too: a reset can never claim the work is about to run an
	// attempt that cannot exist.
	after := cloneCheckpoint(wedged)
	reset, ok := reprepared.StageReset("build.plan.approval", reloaded, after)
	if !ok {
		t.Fatalf("the reset was not derivable")
	}
	if reset.NextWorkAttempt != math.MaxInt {
		t.Fatalf("evidence next work attempt = %d, want a saturated %d", reset.NextWorkAttempt, math.MaxInt)
	}
}
