package plan

import (
	"strings"
	"testing"

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
		{Kind: CommandKindActivateNode, NodeID: "implement.plan", TargetNodeID: "implement.do", NodeStatus: state.NodeStatusReady, Key: "run_1/activate_node/implement.plan/to/implement.do"},
	})
}

func TestPlanCompletesParentAfterLastGate(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.plan":       {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e1"}},
		"implement.do":         {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e2"}},
		"implement.test.tests": {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e3"}},
		"implement.review":     {Status: state.NodeStatusCompleted, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass", EvidenceRef: "e4"}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindActivateNode, NodeID: "implement.review", TargetNodeID: "implement.done", NodeStatus: state.NodeStatusCompleted, Key: "run_1/activate_node/implement.review/to/implement.done"},
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

func TestPlanPoisonsFailedGateImmediately(t *testing.T) {
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.test.tests": {Status: state.NodeStatusFailed, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "fail"}},
	})
	got, err := Plan(st, compoundTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NodeID != "implement.test.tests" || got[0].TargetNodeID != "implement" {
		t.Fatalf("commands = %#v", got)
	}
	if !strings.Contains(got[0].Reason, `gate "implement.test.tests" failed`) {
		t.Fatalf("reason = %q", got[0].Reason)
	}
}

func TestPlanClaimedDoneFlipRoutesAsFailure(t *testing.T) {
	// A child whose status flipped to failed despite a recorded pass outcome
	// (claimed done without evidence) must route through the failure path.
	st := expandedPlannerState(map[string]state.NodeState{
		"implement.review": {Status: state.NodeStatusFailed, Attempt: 1, ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass"}},
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

func TestPlanGateSettleDoesNotAdvertiseUnhonoredBudget(t *testing.T) {
	// Gate stages poison on first failure in this phase, so the settle
	// command must publish maxAttempts=1 even when the template declares a
	// gate retry budget — otherwise a command executor and NextAfterStage
	// would disagree.
	tmpl := compoundTemplate()
	node := tmpl.Nodes["implement"]
	node.Review = &model.Step{ID: "review", Retry: &model.RetryPolicy{MaxAttempts: 3}, Performer: model.Performer{Kind: model.PerformerAgent, Prompt: "Review it"}}
	tmpl.Nodes["implement"] = node
	st := expandedPlannerState(map[string]state.NodeState{
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
}

func TestNextAfterStageRejectsDoneAndUnknownChildren(t *testing.T) {
	tmplNode := compoundTemplate().Nodes["implement"]
	specs := model.ExpandNode("implement", tmplNode)
	children := compoundChildren()
	if _, err := NextAfterStage("implement", children, specs, "implement.done", "pass", 1); err == nil {
		t.Fatal("done stage must not be advanceable")
	}
	if _, err := NextAfterStage("implement", children, specs, "implement.nope", "pass", 1); err == nil {
		t.Fatal("unknown child must error")
	}
}
