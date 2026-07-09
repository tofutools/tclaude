package state

import (
	"errors"
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
	original := st
	_, err = Apply(st, Event{
		Type:   EventNodeBlocked,
		NodeID: "implement",
		Reason: "blocked",
		Owner:  "human:johan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if original.Nodes["implement"].Status != st.Nodes["implement"].Status {
		t.Fatal("input state mutated")
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

	_, err = Decode([]byte(`{"stateSchemaVersion":1,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":"","extra":true}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":999,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":""}`))
	if !errors.Is(err, ErrNewerSchemaVersion) {
		t.Fatalf("expected newer schema error, got %v", err)
	}

	_, err = Decode([]byte(`{"stateSchemaVersion":0,"status":"running","originalTemplateRef":"a","currentTemplateRef":"a","nodes":{},"lastLogSeq":0,"logChecksum":""}`))
	if !errors.Is(err, ErrInvalidSchemaVersion) {
		t.Fatalf("expected invalid schema error, got %v", err)
	}
}

func TestActorRefValidation(t *testing.T) {
	valid := []ActorRef{"human:johan", "agent:agt_123abc", "program:go test ./...@exit0", "program:script@exit-1"}
	for _, actor := range valid {
		if !ValidateActorRef(actor) {
			t.Fatalf("expected actor %q to be valid", actor)
		}
	}
	invalid := []ActorRef{"", "human:", "agent:123", "program:cmd", "other:x"}
	for _, actor := range invalid {
		if ValidateActorRef(actor) {
			t.Fatalf("expected actor %q to be invalid", actor)
		}
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
		},
	}
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
