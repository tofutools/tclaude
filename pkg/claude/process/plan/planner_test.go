package plan

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

var testTime = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func TestPlanGoldenCommands(t *testing.T) {
	tests := []struct {
		name string
		st   *state.State
		want []commandWant
	}{
		{
			name: "ready task starts next attempt",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusReady},
				"decide":    {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
				"failed":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindStartAttempt, NodeID: "implement", Attempt: 1, Key: "run_1/start_attempt/implement/attempt-1/start"},
			},
		},
		{
			name: "completed task activates pass edge",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {
					Type:          model.NodeTypeTask,
					Status:        state.NodeStatusCompleted,
					Attempt:       1,
					ActiveAttempt: &state.AttemptState{Attempt: 1, Outcome: "pass"},
				},
				"decide": {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
				"failed": {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindActivateNode, NodeID: "implement", TargetNodeID: "decide", SourceNodeStatus: state.NodeStatusCompleted, NodeStatus: state.NodeStatusReady, Key: "run_1/activate_node/implement/to/decide"},
			},
		},
		{
			name: "failed task with retry budget readies itself",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusFailed, Attempt: 1},
				"decide":    {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
				"failed":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindActivateNode, NodeID: "implement", TargetNodeID: "implement", NodeStatus: state.NodeStatusReady, Key: "run_1/activate_node/implement/retry/attempt-2"},
			},
		},
		{
			name: "failed task without remaining retry completes run failed",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusFailed, Attempt: 2},
				"decide":    {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
				"failed":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindCompleteRun, NodeID: "implement", RunStatus: state.RunStatusFailed, Key: "run_1/complete_run/implement/failed"},
			},
		},
		{
			name: "completed task with failure outcome completes run failed",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {
					Type:          model.NodeTypeTask,
					Status:        state.NodeStatusCompleted,
					Attempt:       2,
					ActiveAttempt: &state.AttemptState{Attempt: 2, Outcome: "cancelled"},
				},
				"decide": {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
				"failed": {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindCompleteRun, NodeID: "implement", RunStatus: state.RunStatusFailed, Key: "run_1/complete_run/implement/failed"},
			},
		},
		{
			name: "decision activates failed terminal and completes failed",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusCompleted},
				"decide": {
					Type:       model.NodeTypeDecision,
					Status:     state.NodeStatusCompleted,
					ChosenEdge: "reject",
					Decisions:  []state.DecisionRecord{{Actor: "human:johan", Verdict: "reject"}},
				},
				"failed": {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindActivateNode, NodeID: "decide", TargetNodeID: "failed", SourceNodeStatus: state.NodeStatusCompleted, NodeStatus: state.NodeStatusCompleted, Key: "run_1/activate_node/decide/to/failed"},
				{Kind: CommandKindCompleteRun, NodeID: "failed", RunStatus: state.RunStatusFailed, Key: "run_1/complete_run/failed/failed"},
			},
		},
		{
			name: "ready decision asks for decision",
			st: stateWithNodes(map[string]state.NodeState{
				"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusCompleted},
				"decide":    {Type: model.NodeTypeDecision, Status: state.NodeStatusReady},
				"failed":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
			}),
			want: []commandWant{
				{Kind: CommandKindRecordDecision, NodeID: "decide", Key: "run_1/record_decision/decide/decision"},
			},
		},
		{
			name: "observed running task settles attempt",
			st: func() *state.State {
				st := stateWithNodes(map[string]state.NodeState{
					"implement": {
						Type:          model.NodeTypeTask,
						Status:        state.NodeStatusRunning,
						Attempt:       1,
						ActiveAttempt: &state.AttemptState{Attempt: 1, CommandID: "cmd_source"},
					},
					"decide": {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
					"failed": {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
					"end":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
				})
				st.OutstandingCommands["cmd_source"] = state.OutstandingCommand{
					ID:     "cmd_source",
					NodeID: "implement",
					Kind:   state.CommandKindStartAttempt,
					Status: state.CommandStatusObserved,
				}
				return st
			}(),
			want: []commandWant{
				{Kind: CommandKindSettleAttempt, NodeID: "implement", SourceCommandID: "cmd_source", Attempt: 1, MaxAttempts: 2, Key: "run_1/settle_attempt/implement/attempt-1/settle"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Plan(tt.st, plannerTemplate())
			if err != nil {
				t.Fatal(err)
			}
			assertCommands(t, got, tt.want)
		})
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	st := stateWithNodes(map[string]state.NodeState{
		"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		"decide":    {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
		"failed":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})

	first, err := Plan(st, plannerTemplate())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Plan(st, plannerTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("planner output changed\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if len(first) != 1 || first[0].ID == "" || first[0].IdempotencyKey == "" {
		t.Fatalf("missing deterministic identifiers: %#v", first)
	}
}

func TestPlanSkipsOutstandingCommand(t *testing.T) {
	st := stateWithNodes(map[string]state.NodeState{
		"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		"decide":    {Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
		"failed":    {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		"end":       {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	commands, err := Plan(st, plannerTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %#v", commands)
	}
	outstanding, err := commands[0].OutstandingCommand(testTime)
	if err != nil {
		t.Fatal(err)
	}
	st.OutstandingCommands[commands[0].ID] = outstanding

	commands, err = Plan(st, plannerTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("expected outstanding command to be skipped, got %#v", commands)
	}
}

func TestPlanWaitTimerCommand(t *testing.T) {
	st := stateWithNodes(map[string]state.NodeState{
		"wait": {Type: model.NodeTypeWait, Status: state.NodeStatusReady},
	})
	tmpl := &model.Template{
		ID:    "wait-demo",
		Start: "wait",
		Nodes: map[string]model.Node{
			"wait": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: "5m"}},
		},
	}
	got, err := Plan(st, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindSetTimer, NodeID: "wait", WaitKind: state.WaitKindTimer, Key: "run_1/set_timer/wait/timer"},
	})
}

func TestPlanWaitSignalHashesUnsafeKeySegment(t *testing.T) {
	st := stateWithNodes(map[string]state.NodeState{
		"wait": {Type: model.NodeTypeWait, Status: state.NodeStatusReady},
	})
	tmpl := &model.Template{
		ID:    "wait-demo",
		Start: "wait",
		Nodes: map[string]model.Node{
			"wait": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Signal: "deploy/prod"}},
		},
	}
	got, err := Plan(st, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("commands = %#v", got)
	}
	if got[0].Kind != CommandKindWaitSignal || got[0].Signal != "deploy/prod" {
		t.Fatalf("command = %#v", got[0])
	}
	if strings.Contains(got[0].IdempotencyKey, "deploy/prod") || !strings.Contains(got[0].IdempotencyKey, "sha256-") {
		t.Fatalf("unsafe idempotency key = %q", got[0].IdempotencyKey)
	}
}

func TestPlanSatisfiedWaitActivatesNext(t *testing.T) {
	st := stateWithNodes(map[string]state.NodeState{
		"wait": {Type: model.NodeTypeWait, Status: state.NodeStatusReady},
		"done": {Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Timers = map[string]state.TimerRecord{
		"timer_1": {ID: "timer_1", NodeID: "wait", Status: state.WaitStatusSatisfied},
	}
	tmpl := &model.Template{
		ID:    "wait-demo",
		Start: "wait",
		Nodes: map[string]model.Node{
			"wait": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: "5m"}, Next: model.Next{"next": "done"}},
			"done": {Type: model.NodeTypeEnd},
		},
	}

	got, err := Plan(st, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, got, []commandWant{
		{Kind: CommandKindActivateNode, NodeID: "wait", TargetNodeID: "done", SourceNodeStatus: state.NodeStatusCompleted, NodeStatus: state.NodeStatusCompleted, Key: "run_1/activate_node/wait/to/done"},
		{Kind: CommandKindCompleteRun, NodeID: "done", RunStatus: state.RunStatusCompleted, Key: "run_1/complete_run/done/completed"},
	})
}

func TestPlanDoesNotEmitForPausedDirtyOrInconsistentRuns(t *testing.T) {
	for _, status := range []state.RunStatus{state.RunStatusPaused, state.RunStatusDirty, state.RunStatusInconsistent} {
		t.Run(string(status), func(t *testing.T) {
			st := stateWithNodes(map[string]state.NodeState{
				"implement": {Type: model.NodeTypeTask, Status: state.NodeStatusReady},
			})
			st.Status = status
			got, err := Plan(st, plannerTemplate())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("expected no commands for %s, got %#v", status, got)
			}
		})
	}
}

type commandWant struct {
	Kind             CommandKind
	NodeID           string
	TargetNodeID     string
	SourceCommandID  string
	SourceNodeStatus state.NodeStatus
	Attempt          int
	MaxAttempts      int
	Key              string
	NodeStatus       state.NodeStatus
	RunStatus        state.RunStatus
	WaitKind         state.WaitKind
}

func assertCommands(t *testing.T, got []Command, want []commandWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("command len = %d, want %d\ncommands = %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind ||
			got[i].NodeID != want[i].NodeID ||
			got[i].TargetNodeID != want[i].TargetNodeID ||
			got[i].SourceCommandID != want[i].SourceCommandID ||
			got[i].SourceNodeStatus != want[i].SourceNodeStatus ||
			got[i].Attempt != want[i].Attempt ||
			got[i].MaxAttempts != want[i].MaxAttempts ||
			got[i].IdempotencyKey != want[i].Key ||
			got[i].NodeStatus != want[i].NodeStatus ||
			got[i].RunStatus != want[i].RunStatus ||
			got[i].WaitKind != want[i].WaitKind {
			t.Fatalf("command[%d] = %#v, want %#v", i, got[i], want[i])
		}
		if got[i].ID == "" {
			t.Fatalf("command[%d] has empty id", i)
		}
	}
}

func plannerTemplate() *model.Template {
	return &model.Template{
		ID:    "manual-demo",
		Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerHuman,
					Ask:  "Implement the change",
				},
				Retry: &model.RetryPolicy{MaxAttempts: 2},
				Next:  model.Next{"pass": "decide"},
			},
			"decide": {
				Type: model.NodeTypeDecision,
				Performer: &model.Performer{
					Kind: model.PerformerHuman,
					Ask:  "Ship it?",
				},
				Next: model.Next{"approve": "end", "reject": "failed"},
			},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
			"end":    {Type: model.NodeTypeEnd},
		},
	}
}

func stateWithNodes(nodes map[string]state.NodeState) *state.State {
	return &state.State{
		RunID:               "run_1",
		Status:              state.RunStatusRunning,
		OriginalTemplateRef: "manual-demo@sha256:test",
		CurrentTemplateRef:  "manual-demo@sha256:test",
		Nodes:               nodes,
		OutstandingCommands: map[string]state.OutstandingCommand{},
		Timers:              map[string]state.TimerRecord{},
		Waits:               map[string]state.WaitRecord{},
	}
}
