package state

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

var testTime = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func TestReducerSequences(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		assert func(t *testing.T, st State)
	}{
		{
			name: "attempt started command issued and settled",
			events: []Event{
				initEvent(),
				{
					Type:      EventNodeAttemptStarted,
					Seq:       2,
					At:        testTime,
					NodeID:    "implement",
					Actor:     "agent:agt_dev123",
					CommandID: "cmd_1",
				},
				{
					Type: EventCommandIssued,
					Seq:  3,
					At:   testTime.Add(time.Minute),
					Command: &OutstandingCommand{
						ID:      "cmd_1",
						NodeID:  "implement",
						Attempt: 1,
						Kind:    "agent.spawn",
					},
				},
				{
					Type:        EventCommandObserved,
					Seq:         4,
					CommandID:   "cmd_1",
					ExternalRef: "agent:agt_dev123",
				},
				{
					Type:    EventNodeAttemptSettled,
					Seq:     5,
					At:      testTime.Add(2 * time.Minute),
					NodeID:  "implement",
					Outcome: "pass",
				},
			},
			assert: func(t *testing.T, st State) {
				node := st.Nodes["implement"]
				if node.Status != NodeStatusCompleted {
					t.Fatalf("node status = %q", node.Status)
				}
				if node.ActiveAttempt == nil || node.ActiveAttempt.Outcome != "pass" {
					t.Fatalf("active attempt = %#v", node.ActiveAttempt)
				}
				if st.OutstandingCommands["cmd_1"].ExternalRef != "agent:agt_dev123" {
					t.Fatalf("command = %#v", st.OutstandingCommands["cmd_1"])
				}
				if st.LastLogSeq != 5 {
					t.Fatalf("last log seq = %d", st.LastLogSeq)
				}
			},
		},
		{
			name: "decision recorded",
			events: []Event{
				initEvent(),
				{
					Type:       EventDecisionRecorded,
					Seq:        2,
					At:         testTime,
					NodeID:     "decide",
					ChosenEdge: "approve",
					Decision: &DecisionRecord{
						Actor:       "human:johan",
						Verdict:     "approve",
						EvidenceRef: "node/decide/log.jsonl#1",
					},
				},
			},
			assert: func(t *testing.T, st State) {
				node := st.Nodes["decide"]
				if node.Status != NodeStatusCompleted {
					t.Fatalf("node status = %q", node.Status)
				}
				if node.ChosenEdge != "approve" || len(node.Decisions) != 1 {
					t.Fatalf("decision state = %#v", node)
				}
				if node.Decisions[0].Timestamp != testTime {
					t.Fatalf("timestamp = %v", node.Decisions[0].Timestamp)
				}
			},
		},
		{
			name: "wait created and satisfied",
			events: []Event{
				initEvent(),
				{
					Type: EventWaitCreated,
					Seq:  2,
					Wait: &WaitRecord{
						ID:       "wait_1",
						NodeID:   "wait-human",
						Kind:     WaitKindHuman,
						Assignee: "human:johan",
					},
				},
				{
					Type:   EventWaitSatisfied,
					Seq:    3,
					At:     testTime,
					WaitID: "wait_1",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Waits["wait_1"].Status != WaitStatusSatisfied {
					t.Fatalf("wait = %#v", st.Waits["wait_1"])
				}
				if st.Nodes["wait-human"].Status != NodeStatusReady {
					t.Fatalf("node = %#v", st.Nodes["wait-human"])
				}
			},
		},
		{
			name: "timer created and satisfied",
			events: []Event{
				initEvent(),
				{
					Type: EventTimerCreated,
					Seq:  2,
					Timer: &TimerRecord{
						ID:     "timer_1",
						NodeID: "timer",
						DueAt:  testTime.Add(time.Hour),
					},
				},
				{
					Type:    EventTimerSatisfied,
					Seq:     3,
					At:      testTime,
					TimerID: "timer_1",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Timers["timer_1"].Status != WaitStatusSatisfied {
					t.Fatalf("timer = %#v", st.Timers["timer_1"])
				}
				if st.Nodes["timer"].Status != NodeStatusReady {
					t.Fatalf("node = %#v", st.Nodes["timer"])
				}
			},
		},
		{
			name: "failed outcome settles failed",
			events: []Event{
				initEvent(),
				{
					Type:   EventNodeAttemptStarted,
					Seq:    2,
					NodeID: "implement",
					Actor:  "agent:agt_dev123",
				},
				{
					Type:    EventNodeAttemptSettled,
					Seq:     3,
					NodeID:  "implement",
					Outcome: "fail",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Nodes["implement"].Status != NodeStatusFailed {
					t.Fatalf("node = %#v", st.Nodes["implement"])
				}
			},
		},
		{
			name: "explicit settled status override",
			events: []Event{
				initEvent(),
				{
					Type:   EventNodeAttemptStarted,
					Seq:    2,
					NodeID: "implement",
					Actor:  "agent:agt_dev123",
				},
				{
					Type:       EventNodeAttemptSettled,
					Seq:        3,
					NodeID:     "implement",
					NodeStatus: NodeStatusBlocked,
					Outcome:    "needs-human",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Nodes["implement"].Status != NodeStatusBlocked {
					t.Fatalf("node = %#v", st.Nodes["implement"])
				}
			},
		},
		{
			name: "blocked unblocked and admin repair",
			events: []Event{
				initEvent(),
				{
					Type:   EventNodeBlocked,
					Seq:    2,
					NodeID: "implement",
					Reason: "needs credentials",
					Owner:  "human:johan",
				},
				{
					Type:        EventAdminRepairRecorded,
					Seq:         3,
					At:          testTime,
					Actor:       "human:johan",
					Reason:      "credentials added",
					RunStatus:   RunStatusDirty,
					EvidenceRef: "admin/repair#1",
				},
				{
					Type:   EventNodeUnblocked,
					Seq:    4,
					NodeID: "implement",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Status != RunStatusDirty {
					t.Fatalf("run status = %q", st.Status)
				}
				if len(st.AdminRecords) != 1 {
					t.Fatalf("admin records = %#v", st.AdminRecords)
				}
				node := st.Nodes["implement"]
				if node.Status != NodeStatusReady || node.BlockedReason != "" || node.BlockedOwner != "" {
					t.Fatalf("node = %#v", node)
				}
			},
		},
		{
			name: "template divergence marker",
			events: []Event{
				initEvent(),
				{
					Type:               EventTemplateDivergenceMarked,
					Seq:                2,
					At:                 testTime,
					Actor:              "human:johan",
					Reason:             "manual unlock migration",
					CurrentTemplateRef: "demo@sha256:new",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.CurrentTemplateRef != "demo@sha256:new" {
					t.Fatalf("current template = %q", st.CurrentTemplateRef)
				}
				if st.TemplateDivergence == nil || !st.TemplateDivergence.Diverged {
					t.Fatalf("template divergence = %#v", st.TemplateDivergence)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := ApplyAll(State{}, tt.events)
			if err != nil {
				t.Fatal(err)
			}
			tt.assert(t, st)
		})
	}
}

func TestReducerDoesNotMutateInput(t *testing.T) {
	st, err := Apply(State{}, initEvent())
	if err != nil {
		t.Fatal(err)
	}
	original := Clone(st)
	_, err = Apply(st, Event{
		Type:   EventNodeBlocked,
		Seq:    2,
		NodeID: "implement",
		Reason: "blocked",
		Owner:  "human:johan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, st) {
		t.Fatal("input state mutated")
	}
	if st.Nodes["implement"].Status != NodeStatusPending {
		t.Fatalf("pre-state changed to %q", st.Nodes["implement"].Status)
	}
}

func TestCloneDeepCopiesState(t *testing.T) {
	st, err := ApplyAll(State{}, []Event{
		initEvent(),
		{
			Type:   EventNodeAttemptStarted,
			Seq:    2,
			NodeID: "implement",
			Actor:  "agent:agt_dev123",
		},
		{
			Type: EventWaitCreated,
			Seq:  3,
			Wait: &WaitRecord{ID: "wait_1", NodeID: "wait-human", Kind: WaitKindHuman, Status: WaitStatusPending},
		},
		{
			Type:    EventCommandIssued,
			Seq:     4,
			Command: &OutstandingCommand{ID: "cmd_1", NodeID: "implement", Kind: "agent.spawn"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clone := Clone(st)
	clone.Nodes["implement"].ActiveAttempt.Actor = "agent:agt_other"
	clone.Waits["wait_1"] = WaitRecord{ID: "wait_1", NodeID: "wait-human", Kind: WaitKindAgent, Status: WaitStatusCanceled}
	clone.OutstandingCommands["cmd_1"] = OutstandingCommand{ID: "cmd_1", NodeID: "implement", Kind: "changed"}

	if st.Nodes["implement"].ActiveAttempt.Actor != "agent:agt_dev123" {
		t.Fatal("node active attempt aliased")
	}
	if st.Waits["wait_1"].Kind != WaitKindHuman {
		t.Fatal("wait map aliased")
	}
	if st.OutstandingCommands["cmd_1"].Kind != "agent.spawn" {
		t.Fatal("command map aliased")
	}
}

func TestReducerErrors(t *testing.T) {
	base, err := Apply(State{}, initEvent())
	if err != nil {
		t.Fatal(err)
	}
	running, err := Apply(base, Event{Type: EventNodeAttemptStarted, Seq: 2, NodeID: "implement", Actor: "agent:agt_dev123"})
	if err != nil {
		t.Fatal(err)
	}
	decisionDone, err := Apply(base, Event{
		Type:       EventDecisionRecorded,
		Seq:        2,
		NodeID:     "decide",
		ChosenEdge: "approve",
		Decision:   &DecisionRecord{Actor: "human:johan", Verdict: "approve"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		st    State
		event Event
		want  string
	}{
		{name: "reinit", st: base, event: Event{Type: EventRunInitialized, Seq: 11, RunID: "run_2", OriginalTemplateRef: "demo@sha256:old", CurrentTemplateRef: "demo@sha256:old"}, want: "already initialized"},
		{name: "seq regression", st: withLastSeq(base, 10), event: Event{Type: EventRunStatusSet, Seq: 3, RunStatus: RunStatusRunning}, want: "must be greater"},
		{name: "duplicate seq", st: withLastSeq(base, 10), event: Event{Type: EventRunStatusSet, Seq: 10, RunStatus: RunStatusRunning}, want: "must be greater"},
		{name: "unknown event type", st: base, event: Event{Type: "future", Seq: 11}, want: "unsupported"},
		{name: "status set without status", st: base, event: Event{Type: EventRunStatusSet, Seq: 11}, want: "requires runStatus"},
		{name: "invalid run status", st: base, event: Event{Type: EventRunStatusSet, Seq: 11, RunStatus: "bogus"}, want: "invalid run status"},
		{name: "undeclared node", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, NodeID: "missing"}, want: "not declared"},
		{name: "start while running", st: running, event: Event{Type: EventNodeAttemptStarted, Seq: 11, NodeID: "implement", Actor: "agent:agt_dev123"}, want: "active attempt"},
		{name: "attempt regression", st: baseWithAttempt(2), event: Event{Type: EventNodeAttemptStarted, Seq: 11, NodeID: "implement", Attempt: 1, Actor: "agent:agt_dev123"}, want: "must be greater"},
		{name: "settle without outcome", st: running, event: Event{Type: EventNodeAttemptSettled, Seq: 11, NodeID: "implement"}, want: "requires outcome"},
		{name: "invalid node status override", st: running, event: Event{Type: EventNodeAttemptSettled, Seq: 11, NodeID: "implement", Outcome: "pass", NodeStatus: "bogus"}, want: "invalid node status"},
		{name: "second decision", st: decisionDone, event: Event{Type: EventDecisionRecorded, Seq: 11, NodeID: "decide", ChosenEdge: "reject", Decision: &DecisionRecord{Actor: "human:johan", Verdict: "reject"}}, want: "already completed"},
		{name: "unknown wait", st: base, event: Event{Type: EventWaitSatisfied, Seq: 11, WaitID: "missing"}, want: "not declared"},
		{name: "invalid wait kind", st: base, event: Event{Type: EventWaitCreated, Seq: 11, Wait: &WaitRecord{ID: "wait", NodeID: "wait-human", Kind: "bogus"}}, want: "invalid wait kind"},
		{name: "invalid wait status", st: base, event: Event{Type: EventWaitCreated, Seq: 11, Wait: &WaitRecord{ID: "wait", NodeID: "wait-human", Kind: WaitKindHuman, Status: "bogus"}}, want: "invalid wait status"},
		{name: "unknown timer", st: base, event: Event{Type: EventTimerSatisfied, Seq: 11, TimerID: "missing"}, want: "not declared"},
		{name: "command without id", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{NodeID: "implement", Kind: "agent.spawn"}}, want: "command id"},
		{name: "invalid command status", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{ID: "cmd", NodeID: "implement", Kind: "agent.spawn", Status: "bogus"}}, want: "invalid command status"},
		{name: "unknown command observed", st: base, event: Event{Type: EventCommandObserved, Seq: 11, CommandID: "missing"}, want: "not outstanding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Apply(tt.st, tt.event)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestInvariants(t *testing.T) {
	tests := []struct {
		name string
		st   State
		code string
	}{
		{
			name: "waiting node without wait",
			st: stateWithNodes(map[string]NodeState{
				"a": {Status: NodeStatusWaitingHuman},
			}),
			code: "waiting_node_without_wait",
		},
		{
			name: "running attempt without command or actor",
			st: stateWithNodes(map[string]NodeState{
				"a": {Status: NodeStatusRunning, ActiveAttempt: &AttemptState{Attempt: 1}},
			}),
			code: "running_attempt_without_command_or_actor",
		},
		{
			name: "completed decision without chosen edge",
			st: stateWithNodes(map[string]NodeState{
				"a": {Type: model.NodeTypeDecision, Status: NodeStatusCompleted},
			}),
			code: "completed_decision_without_one_chosen_edge",
		},
		{
			name: "blocked node without reason owner",
			st: stateWithNodes(map[string]NodeState{
				"a": {Status: NodeStatusBlocked, BlockedReason: "blocked"},
			}),
			code: "blocked_node_without_reason_owner",
		},
		{
			name: "invalid decision actor",
			st: stateWithNodes(map[string]NodeState{
				"a": {
					Type:       model.NodeTypeDecision,
					Status:     NodeStatusCompleted,
					ChosenEdge: "approve",
					Decisions:  []DecisionRecord{{Actor: "bad", Verdict: "approve"}},
				},
			}),
			code: "invalid_decision_actor",
		},
		{
			name: "waiting human with timer wait",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{
					"a": {Status: NodeStatusWaitingHuman},
				})
				st.Waits["wait"] = WaitRecord{ID: "wait", NodeID: "a", Kind: WaitKindTimer, Status: WaitStatusPending}
				return st
			}(),
			code: "waiting_node_without_wait",
		},
		{
			name: "completed decision with mismatched chosen edge",
			st: stateWithNodes(map[string]NodeState{
				"a": {
					Type:       model.NodeTypeDecision,
					Status:     NodeStatusCompleted,
					ChosenEdge: "approve",
					Decisions:  []DecisionRecord{{Actor: "human:johan", Verdict: "reject"}},
				},
			}),
			code: "completed_decision_without_one_chosen_edge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diagnostics := CheckInvariants(&tt.st)
			if !hasDiagnostic(diagnostics, tt.code) {
				t.Fatalf("expected %q, got %#v", tt.code, diagnostics)
			}
		})
	}
}

func TestInvariantsPassForValidState(t *testing.T) {
	st := stateWithNodes(map[string]NodeState{
		"wait": {
			Status: NodeStatusWaitingHuman,
		},
		"run": {
			Status:        NodeStatusRunning,
			ActiveAttempt: &AttemptState{Attempt: 1, Actor: "agent:agt_dev123"},
		},
		"decision": {
			Type:       model.NodeTypeDecision,
			Status:     NodeStatusCompleted,
			ChosenEdge: "approve",
			Decisions: []DecisionRecord{{
				Actor:     "program:go-test@exit0",
				Verdict:   "approve",
				Timestamp: testTime,
			}},
		},
		"blocked": {
			Status:        NodeStatusBlocked,
			BlockedReason: "needs review",
			BlockedOwner:  "human:johan",
		},
	})
	st.Waits["wait_1"] = WaitRecord{ID: "wait_1", NodeID: "wait", Kind: WaitKindHuman, Status: WaitStatusPending}

	if diagnostics := CheckInvariants(&st); diagnostics.HasErrors() {
		t.Fatalf("unexpected invariant errors: %#v", diagnostics)
	}
}

func TestCheckInvariantsNilStateReportsOnce(t *testing.T) {
	diagnostics := CheckInvariants(nil)
	if len(diagnostics) != 1 || diagnostics[0].Code != "nil_state" {
		t.Fatalf("expected one nil_state diagnostic, got %#v", diagnostics)
	}
}

func TestJSONRoundTripStrictUnknownAndSchemaVersion(t *testing.T) {
	st, err := Apply(State{}, initEvent())
	if err != nil {
		t.Fatal(err)
	}
	st.LogChecksum = "sha256:abc"
	data, err := Encode(&st)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.StateSchemaVersion != StateSchemaVersion {
		t.Fatalf("schema version = %d", roundTrip.StateSchemaVersion)
	}
	if roundTrip.LogChecksum != st.LogChecksum {
		t.Fatalf("log checksum = %q", roundTrip.LogChecksum)
	}
	if strings.Contains(string(data), "0001-01-01") {
		t.Fatalf("encoded state contains zero time noise:\n%s", data)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":1,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":"","extra":true}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":999,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":""}`))
	if !errors.Is(err, ErrNewerSchemaVersion) {
		t.Fatalf("expected newer schema error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":999,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":"","futureField":true}`))
	if !errors.Is(err, ErrNewerSchemaVersion) {
		t.Fatalf("expected newer schema error before unknown-field error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":0,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":""}`))
	if !errors.Is(err, ErrInvalidSchemaVersion) {
		t.Fatalf("expected invalid schema error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":1,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{"a":{"status":"pending","extra":true}},"lastLogSeq":0,"logChecksum":""}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected nested unknown field error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":1,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":""} {}`))
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("expected multiple JSON values error, got %v", err)
	}
}

func TestActorRefValidation(t *testing.T) {
	valid := []ActorRef{"human:johan", "agent:agt_123abc", "program:go test ./...@exit0", "program:script@exit-1"}
	for _, actor := range valid {
		if !ValidateActorRef(actor) {
			t.Fatalf("expected actor %q to be valid", actor)
		}
	}
	invalid := []ActorRef{"", " human:johan ", "human:", "agent:123", "program:cmd", "other:x"}
	for _, actor := range invalid {
		if ValidateActorRef(actor) {
			t.Fatalf("expected actor %q to be invalid", actor)
		}
	}
}

func TestApplyNormalizesActorRefs(t *testing.T) {
	st, err := ApplyAll(State{}, []Event{
		initEvent(),
		{Type: EventNodeAttemptStarted, Seq: 2, NodeID: "implement", Actor: " agent:agt_dev123 "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes["implement"].ActiveAttempt.Actor != "agent:agt_dev123" {
		t.Fatalf("actor = %q", st.Nodes["implement"].ActiveAttempt.Actor)
	}
}

func initEvent() Event {
	return Event{
		Type:                EventRunInitialized,
		Seq:                 1,
		RunID:               "run_1",
		OriginalTemplateRef: "demo@sha256:old",
		CurrentTemplateRef:  "demo@sha256:old",
		Nodes: []NodeInit{
			{ID: "implement", Type: model.NodeTypeTask},
			{ID: "decide", Type: model.NodeTypeDecision},
			{ID: "wait-human", Type: model.NodeTypeWait},
			{ID: "timer", Type: model.NodeTypeWait},
		},
	}
}

func withLastSeq(st State, seq int64) State {
	st.LastLogSeq = seq
	return st
}

func baseWithAttempt(attempt int) State {
	st, err := Apply(State{}, initEvent())
	if err != nil {
		panic(err)
	}
	node := st.Nodes["implement"]
	node.Attempt = attempt
	st.Nodes["implement"] = node
	return st
}

func stateWithNodes(nodes map[string]NodeState) State {
	return State{
		StateSchemaVersion:  StateSchemaVersion,
		Status:              RunStatusRunning,
		OriginalTemplateRef: "demo@sha256:old",
		CurrentTemplateRef:  "demo@sha256:old",
		Nodes:               nodes,
		OutstandingCommands: map[string]OutstandingCommand{},
		Waits:               map[string]WaitRecord{},
		Timers:              map[string]TimerRecord{},
	}
}

func hasDiagnostic(diagnostics Diagnostics, code string) bool {
	for _, diag := range diagnostics {
		if diag.Code == code {
			return true
		}
	}
	return false
}
