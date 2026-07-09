package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

func compoundTemplate() *model.Template {
	return &model.Template{
		ID:    "compound-demo",
		Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Implement the change"},
				Plan:      &model.Step{ID: "plan", Performer: model.Performer{Kind: model.PerformerAgent, Prompt: "Plan it"}},
				Checks:    []model.Step{{ID: "tests", Performer: model.Performer{Kind: model.PerformerProgram, Run: "go test ./..."}}},
				Review:    &model.Step{ID: "review", Performer: model.Performer{Kind: model.PerformerAgent, Profile: "reviewer", Prompt: "Review it"}},
				Retry:     &model.RetryPolicy{MaxAttempts: 2},
				Next:      model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
}

func fixedTime() time.Time {
	return time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
}

func compoundChildren() []string {
	return []string{"implement.plan", "implement.do", "implement.test.tests", "implement.review", "implement.done"}
}

func expandedPlannerState(childStatus map[string]state.NodeState) *state.State {
	nodes := map[string]state.NodeState{
		"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusRunning, Children: compoundChildren()},
		"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	}
	stages := map[string]model.StageKind{
		"implement.plan":       model.StagePlan,
		"implement.do":         model.StageDo,
		"implement.test.tests": model.StageTest,
		"implement.review":     model.StageReview,
		"implement.done":       model.StageDone,
	}
	for _, childID := range compoundChildren() {
		node := state.NodeState{Status: state.NodeStatusPending, Parent: "implement", Stage: stages[childID]}
		if node.Stage == model.StageTest {
			node.StepID = "tests"
		}
		if override, ok := childStatus[childID]; ok {
			override.Parent = "implement"
			override.Stage = node.Stage
			override.StepID = node.StepID
			node = override
		}
		nodes[childID] = node
	}
	return stateWithNodes(nodes)
}

func TestPlanExpandsReadyCompoundNode(t *testing.T) {
	st := stateWithNodes(map[string]state.NodeState{
		"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindExpandNode || got[0].NodeID != "implement" {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].IdempotencyKey != "run_1/expand_node/implement/expand" {
		t.Fatalf("key = %q", got[0].IdempotencyKey)
	}
	ids := make([]string, 0, len(got[0].Children))
	for _, child := range got[0].Children {
		ids = append(ids, child.ID)
	}
	if strings.Join(ids, " ") != strings.Join(compoundChildren(), " ") {
		t.Fatalf("children = %v", ids)
	}
}

func TestPlanStartsReadyStageChildWithStagePerformer(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {Status: state.NodeStatusReady},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindStartAttempt, NodeID: "implement.plan", Attempt: 1, Key: "run_1/start_attempt/implement.plan/attempt-1/start"},
	})
	if got[0].Performer == nil || got[0].Performer.Prompt != "Plan it" {
		t.Fatalf("stage performer = %#v", got[0].Performer)
	}
}

func TestPlanSettlesObservedRunningStageChild(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.do": {
			Status:        state.NodeStatusRunning,
			Attempt:       1,
			ActiveAttempt: &state.AttemptState{Attempt: 1, CommandID: "cmd_source"},
		},
	})
	st.OutstandingCommands["cmd_source"] = state.OutstandingCommand{
		ID: "cmd_source", NodeID: "implement.do", Kind: state.CommandKindStartAttempt, Status: state.CommandStatusObserved,
	}
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindSettleAttempt, NodeID: "implement.do", SourceCommandID: "cmd_source", Attempt: 1, MaxAttempts: 2, Key: "run_1/settle_attempt/implement.do/attempt-1/settle"},
	})
}

func TestPlanActivatesNextStageAfterPass(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {
			Status:        state.NodeStatusCompleted,
			Attempt:       1,
			ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "artifacts/plan.md"},
		},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindActivateNode, NodeID: "implement.plan", TargetNodeID: "implement.do", NodeStatus: state.NodeStatusReady, Key: "run_1/activate_node/implement.plan/to/implement.do/attempt-1"},
	})
}

func TestPlanCompletesParentAfterLastGate(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan":       {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":         {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2"}},
		"implement.test.tests": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e3"}},
		"implement.review": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e4"},
			Decisions: []state.DecisionRecord{{Actor: "agent:agt_reviewer", Verdict: "pass", EvidenceRef: "e4"}}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindActivateNode, NodeID: "implement.review", TargetNodeID: "implement.done", NodeStatus: state.NodeStatusCompleted, Key: "run_1/activate_node/implement.review/to/implement.done/decisions-1"},
	})
}

func TestPlanCompletedCompoundActivatesParentPassEdge(t *testing.T) {
	// The reducer completes the parent atomically with the done stage, so the
	// planner's next round sees a completed parent and fires its pass edge.
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan":       {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":         {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2"}},
		"implement.test.tests": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e3"}},
		"implement.review":     {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e4"}},
		"implement.done":       {Status: state.NodeStatusCompleted},
	})
	parent := st.Nodes["implement"]
	parent.Status = state.NodeStatusCompleted
	st.Nodes["implement"] = parent
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindActivateNode, NodeID: "implement", TargetNodeID: "end", SourceNodeStatus: state.NodeStatusCompleted, NodeStatus: state.NodeStatusCompleted, Key: "run_1/activate_node/implement/to/end"},
		{Kind: CommandKindCompleteRun, NodeID: "end", RunStatus: state.RunStatusCompleted, Key: "run_1/complete_run/end/completed"},
	})
}

func TestPlanRetriesFailedWorkStageWithinBudget(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.do": {Status: state.NodeStatusFailed, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "fail"}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindActivateNode, NodeID: "implement.do", TargetNodeID: "implement.do", NodeStatus: state.NodeStatusReady, Key: "run_1/activate_node/implement.do/retry/attempt-2"},
	})
}

func TestPlanPoisonsExhaustedWorkStage(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.do": {Status: state.NodeStatusFailed, Attempt: 2, ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "fail"}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindBlockNode {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].NodeID != "implement.do" || got[0].TargetNodeID != "implement" {
		t.Fatalf("block command = %#v", got[0])
	}
	if !strings.Contains(got[0].Reason, "exhausted its budget of 2 attempts") || got[0].Owner != DefaultBlockedOwner {
		t.Fatalf("block command = %#v", got[0])
	}
}

func TestPlanPoisonsGateWithExhaustedBudget(t *testing.T) {
	// Default gate budget is 1 failed verdict; the reducer already counted
	// the settling failure, so FailCount 1 means the budget is spent.
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.test.tests": {Status: state.NodeStatusFailed, Attempt: 1, FailCount: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "fail"}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindBlockNode || got[0].NodeID != "implement.test.tests" || got[0].TargetNodeID != "implement" {
		t.Fatalf("commands = %#v", got)
	}
	if !strings.Contains(got[0].Reason, `gate "implement.test.tests" exhausted its budget of 1 failed verdicts`) {
		t.Fatalf("reason = %q", got[0].Reason)
	}
}

// compoundTemplateWithGateBudgets grants the tests gate 3 failed verdicts and
// the review gate 2.
func compoundTemplateWithGateBudgets() *model.Template {
	tmpl := compoundTemplate()
	node := tmpl.Nodes["implement"]
	node.Checks = []model.Step{{ID: "tests", Retry: &model.RetryPolicy{MaxAttempts: 3}, Performer: model.Performer{Kind: model.PerformerProgram, Run: "go test ./..."}}}
	node.Review = &model.Step{ID: "review", Retry: &model.RetryPolicy{MaxAttempts: 2}, Performer: model.Performer{Kind: model.PerformerAgent, Profile: "reviewer", Prompt: "Review it"}}
	tmpl.Nodes["implement"] = node
	return tmpl
}

func TestPlanEmitsGateFeedbackWithinBudget(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":   {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2", SettledAt: fixedTime()}},
		"implement.test.tests": {
			Status: state.NodeStatusFailed, Attempt: 1, FailCount: 1,
			ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "fail", Feedback: "TestFoo fails", EvidenceRef: "artifact:test-log"},
			Decisions:     []state.DecisionRecord{{Actor: "program:go test@exit1", Verdict: "fail", EvidenceRef: "artifact:test-log"}},
		},
	})
	got, err := Plan(st, compoundTemplateWithGateBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindGateFeedback {
		t.Fatalf("commands = %#v", got)
	}
	cmd := got[0]
	if cmd.NodeID != "implement.test.tests" || cmd.TargetNodeID != "implement.do" || cmd.Attempt != 1 {
		t.Fatalf("feedback command = %#v", cmd)
	}
	if cmd.Feedback != "TestFoo fails" || cmd.EvidenceRef != "artifact:test-log" {
		t.Fatalf("feedback payload = %#v", cmd)
	}
	if strings.Join(cmd.Gates, " ") != "implement.test.tests" || len(cmd.ResetCounters) != 0 {
		t.Fatalf("reset span = gates %v counters %v", cmd.Gates, cmd.ResetCounters)
	}
	// The key is generation-scoped by the gate's verdict count so the next
	// loop window plans a fresh command instead of colliding with this slot.
	if cmd.IdempotencyKey != "run_1/gate_feedback/implement.test.tests/feedback/decisions-1" {
		t.Fatalf("key = %q", cmd.IdempotencyKey)
	}
}

func TestPlanReviewFailureResetsCrossKindGateCounters(t *testing.T) {
	// A failed review re-enters do; the tests gate sits inside the reset span
	// and is a different stage kind, so its fail counter resets too.
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan":       {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":         {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2", SettledAt: fixedTime()}},
		"implement.test.tests": {Status: state.NodeStatusCompleted, Attempt: 2, FailCount: 1, ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "pass", EvidenceRef: "e3"}},
		"implement.review": {
			Status: state.NodeStatusFailed, Attempt: 1, FailCount: 1,
			ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "fail", Feedback: "needs a test for the edge case"},
		},
	})
	got, err := Plan(st, compoundTemplateWithGateBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindGateFeedback || got[0].TargetNodeID != "implement.do" {
		t.Fatalf("commands = %#v", got)
	}
	if strings.Join(got[0].Gates, " ") != "implement.test.tests implement.review" {
		t.Fatalf("reset gates = %v", got[0].Gates)
	}
	if strings.Join(got[0].ResetCounters, " ") != "implement.test.tests" {
		t.Fatalf("reset counters = %v", got[0].ResetCounters)
	}
}

func TestPlanPoisonsGateWhenWorkBudgetExhausted(t *testing.T) {
	// The gate has verdicts left, but the do stage has already spent its two
	// attempts, so the loop cannot re-enter and the node poisons.
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":   {Status: state.NodeStatusCompleted, Attempt: 2, ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "pass", EvidenceRef: "e2", SettledAt: fixedTime()}},
		"implement.test.tests": {
			Status: state.NodeStatusFailed, Attempt: 2, FailCount: 2,
			ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "fail"},
		},
	})
	got, err := Plan(st, compoundTemplateWithGateBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindBlockNode || got[0].TargetNodeID != "implement" {
		t.Fatalf("commands = %#v", got)
	}
	if !strings.Contains(got[0].Reason, `stage "implement.do" has exhausted its budget of 2 attempts`) {
		t.Fatalf("reason = %q", got[0].Reason)
	}
}

func TestPlanShortCircuitsReenteredGateOnUnchangedEvidence(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":   {Status: state.NodeStatusCompleted, Attempt: 2, ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "pass", EvidenceRef: "e2", EvidenceHash: hash, SettledAt: fixedTime()}},
		"implement.test.tests": {
			Status: state.NodeStatusReady, Attempt: 1, FailCount: 0, LastEvidenceHash: hash,
			Decisions: []state.DecisionRecord{{Actor: "program:go test@exit0", Verdict: "pass", EvidenceRef: "e3"}},
		},
	})
	got, err := Plan(st, compoundTemplateWithGateBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindShortCircuit || got[0].NodeID != "implement.test.tests" {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].EvidenceHash != hash {
		t.Fatalf("evidence hash = %q", got[0].EvidenceHash)
	}
	if got[0].Performer != nil {
		t.Fatalf("short-circuit must not carry a performer: %#v", got[0].Performer)
	}
}

func TestPlanReenteredGateWithChangedEvidenceStartsAttempt(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":   {Status: state.NodeStatusCompleted, Attempt: 2, ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "pass", EvidenceRef: "e2", EvidenceHash: "hash-new", SettledAt: fixedTime()}},
		"implement.test.tests": {
			Status: state.NodeStatusReady, Attempt: 1, LastEvidenceHash: "hash-old",
			Decisions: []state.DecisionRecord{{Actor: "program:go test@exit1", Verdict: "fail"}},
		},
	})
	got, err := Plan(st, compoundTemplateWithGateBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindStartAttempt || got[0].NodeID != "implement.test.tests" {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].RetryMode != "" || got[0].Feedback != "" {
		t.Fatalf("gate start must not carry retry mode or feedback: %#v", got[0])
	}
}

func TestPlanStartAttemptCarriesRetryModeAndFeedback(t *testing.T) {
	tmpl := compoundTemplate()
	node := tmpl.Nodes["implement"]
	node.Retry = &model.RetryPolicy{MaxAttempts: 2, OnFail: "feedback-same-session"}
	tmpl.Nodes["implement"] = node
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do": {
			Status: state.NodeStatusReady, Attempt: 1,
			ActiveAttempt:   &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2", SettledAt: fixedTime()},
			PendingFeedback: &state.FeedbackRef{FromNodeID: "implement.test.tests", Feedback: "TestFoo fails", EvidenceRef: "artifact:test-log"},
		},
	})
	got, err := Plan(st, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindStartAttempt || got[0].NodeID != "implement.do" || got[0].Attempt != 2 {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].RetryMode != "feedback-same-session" {
		t.Fatalf("retry mode = %q", got[0].RetryMode)
	}
	if got[0].Feedback != "TestFoo fails" || got[0].FeedbackFrom != "implement.test.tests" {
		t.Fatalf("feedback threading = %#v", got[0])
	}
}

func TestPlanClaimedDoneFlipRoutesAsFailure(t *testing.T) {
	// A child whose status flipped to failed despite a recorded pass outcome
	// (claimed done without evidence) must route through the failure path.
	// The reducer counts the flip as a failed verdict, so FailCount is 1 and
	// the default budget is spent.
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.review": {Status: state.NodeStatusFailed, Attempt: 1, FailCount: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass"}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindBlockNode || got[0].TargetNodeID != "implement" {
		t.Fatalf("commands = %#v", got)
	}
}

func TestPlanBlockedCompoundEmitsNothing(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.test.tests": {Status: state.NodeStatusBlocked, BlockedReason: "gate failed", BlockedOwner: DefaultBlockedOwner},
	})
	parent := st.Nodes["implement"]
	parent.Status = state.NodeStatusBlocked
	parent.BlockedReason = "gate failed"
	parent.BlockedOwner = DefaultBlockedOwner
	st.Nodes["implement"] = parent
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("blocked compound must be quiescent, got %#v", got)
	}
}

func TestPlanGateSettleIsWindowTerminal(t *testing.T) {
	// Gate settles always carry maxAttempts=1: a failed gate never re-readies
	// itself — the feedback loop re-enters it via pending — so the settle must
	// not advertise the gate's verdict budget as attempt retries. The settle
	// also records the work-evidence hash the verdict evaluated, which powers
	// the evidence-unchanged short-circuit.
	tmpl := compoundTemplateWithGateBudgets()
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.do":         {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2", EvidenceHash: "work-hash", SettledAt: fixedTime()}},
		"implement.test.tests": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e3"}},
		"implement.review": {
			Status:        state.NodeStatusRunning,
			Attempt:       1,
			ActiveAttempt: &state.AttemptState{Attempt: 1, CommandID: "cmd_source"},
		},
	})
	st.OutstandingCommands["cmd_source"] = state.OutstandingCommand{
		ID: "cmd_source", NodeID: "implement.review", Kind: state.CommandKindStartAttempt, Status: state.CommandStatusObserved,
	}
	got, err := Plan(st, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != CommandKindSettleAttempt || got[0].MaxAttempts != 1 {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].WorkEvidenceHash != "work-hash" {
		t.Fatalf("work evidence hash = %q", got[0].WorkEvidenceHash)
	}
}

func TestNextAfterStageRejectsDoneAndUnknownChildren(t *testing.T) {
	tmplNode := compoundTemplate().Nodes["implement"]
	specs := model.ExpandNode("implement", tmplNode)
	children := compoundChildren()
	if _, err := NextAfterStage("implement", children, specs, nil, StageSettle{ChildID: "implement.done", Outcome: "pass", Attempt: 1}); err == nil {
		t.Fatal("done stage must not be advanceable")
	}
	if _, err := NextAfterStage("implement", children, specs, nil, StageSettle{ChildID: "implement.nope", Outcome: "pass", Attempt: 1}); err == nil {
		t.Fatal("unknown child must error")
	}
}
