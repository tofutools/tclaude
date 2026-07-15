package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

var testTime = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func blockCommandForTest(id, nodeID string, attempt int, owner string, status CommandStatus) *OutstandingCommand {
	payload, err := json.Marshal(struct {
		ID      string      `json:"id"`
		Kind    CommandKind `json:"kind"`
		RunID   string      `json:"runId"`
		NodeID  string      `json:"nodeId"`
		Attempt int         `json:"attempt"`
		Owner   string      `json:"owner"`
	}{ID: id, Kind: CommandKindBlockNode, RunID: "run", NodeID: nodeID, Attempt: attempt, Owner: owner})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(payload)
	return &OutstandingCommand{
		ID: id, NodeID: nodeID, Attempt: attempt, Kind: CommandKindBlockNode, Status: status,
		Payload: payload, PayloadHash: hex.EncodeToString(sum[:]),
	}
}

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
						Kind:    CommandKindStartAttempt,
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
			name: "settled retry can start next attempt",
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
					Outcome:    "fail",
					NodeStatus: NodeStatusReady,
				},
				{
					Type:   EventNodeAttemptStarted,
					Seq:    4,
					NodeID: "implement",
					Actor:  "agent:agt_dev123",
				},
			},
			assert: func(t *testing.T, st State) {
				node := st.Nodes["implement"]
				if node.Status != NodeStatusRunning || node.Attempt != 2 {
					t.Fatalf("node = %#v", node)
				}
			},
		},
		{
			name: "node status set",
			events: []Event{
				initEvent(),
				{Type: EventNodeStatusSet, Seq: 2, NodeID: "implement", NodeStatus: NodeStatusReady},
			},
			assert: func(t *testing.T, st State) {
				if st.Nodes["implement"].Status != NodeStatusReady {
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
					NodeStatus: NodeStatusReady,
					Outcome:    "retry",
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Nodes["implement"].Status != NodeStatusReady {
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
					At:     testTime,
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
					Type:        EventBlockResolutionRecorded,
					Seq:         4,
					At:          testTime,
					Actor:       "human:johan",
					Reason:      "credentials added",
					EvidenceRef: "admin/repair#1",
					Resolution:  &BlockResolution{NodeID: "implement", BlockedAttempt: 1, Decision: BlockDecisionRetry, Actor: "human:johan", Reason: "credentials added", EvidenceRef: "admin/repair#1", Timestamp: testTime},
				},
				{
					Type:       EventNodeUnblocked,
					Seq:        5,
					At:         testTime,
					NodeID:     "implement",
					NodeStatus: NodeStatusReady,
					Resolution: &BlockResolution{NodeID: "implement", BlockedAttempt: 1, Decision: BlockDecisionRetry, Actor: "human:johan", Reason: "credentials added", EvidenceRef: "admin/repair#1", Timestamp: testTime},
				},
			},
			assert: func(t *testing.T, st State) {
				if st.Status != RunStatusDirty {
					t.Fatalf("run status = %q", st.Status)
				}
				if len(st.AdminRecords) != 2 || st.AdminRecords[0].Type != EventAdminRepairRecorded || st.AdminRecords[1].Type != EventBlockResolutionRecorded {
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

func TestSuccessfulReducerEventsPreserveInvariants(t *testing.T) {
	for _, events := range [][]Event{
		{
			initEvent(),
			{Type: EventNodeAttemptStarted, Seq: 2, NodeID: "implement", Actor: "agent:agt_dev123"},
			{Type: EventNodeAttemptSettled, Seq: 3, NodeID: "implement", Outcome: "pass"},
		},
		{
			initEvent(),
			{Type: EventDecisionRecorded, Seq: 2, NodeID: "decide", ChosenEdge: "approve", Decision: &DecisionRecord{Actor: "human:johan", Verdict: "approve"}},
		},
		{
			initEvent(),
			{Type: EventWaitCreated, Seq: 2, Wait: &WaitRecord{ID: "wait_1", NodeID: "wait-human", Kind: WaitKindHuman, Status: WaitStatusPending}},
			{Type: EventWaitSatisfied, Seq: 3, WaitID: "wait_1"},
		},
		{
			initEvent(),
			{Type: EventCommandIssued, Seq: 2, At: testTime, Command: blockCommandForTest("cmd_block", "implement", 1, "human:johan", CommandStatusIssued)},
			{Type: EventCommandObserved, Seq: 3, At: testTime, CommandID: "cmd_block"},
			{Type: EventContactScheduled, Seq: 4, At: testTime, Contact: &ContactState{
				CommandID: "cmd_block", Kind: WaitKindHuman, Assignee: "human:johan", Cadence: "30m0s", Budget: 5,
				EscalationTarget: "human:operator", NextContactAt: testTime.Add(30 * time.Minute),
			}},
			{Type: EventNodeBlocked, Seq: 5, At: testTime, NodeID: "implement", Attempt: 1, Reason: "needs credentials", Owner: "human:johan"},
		},
	} {
		st := State{}
		for _, event := range events {
			next, err := Apply(st, event)
			if err != nil {
				t.Fatal(err)
			}
			if diagnostics := CheckInvariants(&next); diagnostics.HasErrors() {
				t.Fatalf("event %#v produced invariant errors: %#v", event, diagnostics.Errors())
			}
			st = next
		}
	}
}

func TestNodeBlockedPersistsFirstEntryTimeAcrossReplay(t *testing.T) {
	st := stateWithNodes(map[string]NodeState{"work": {Status: NodeStatusFailed, Attempt: 2}})
	firstAt := testTime
	blocked, err := Apply(st, Event{
		Type: EventNodeBlocked, Seq: 1, At: firstAt, NodeID: "work", Attempt: 2,
		Reason: "budget exhausted", Owner: "human:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := Apply(blocked, Event{
		Type: EventNodeBlocked, Seq: 2, At: firstAt.Add(time.Hour), NodeID: "work", Attempt: 2,
		Reason: "budget exhausted", Owner: "human:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Nodes["work"].BlockedAt.Equal(firstAt) {
		t.Fatalf("idempotent replay changed blockedAt: %s", replayed.Nodes["work"].BlockedAt)
	}
}

func TestNodeBlockedAcceptsHumanAndRoleOwners(t *testing.T) {
	for _, owner := range []string{"human:operator", "role:oncall"} {
		t.Run(owner, func(t *testing.T) {
			st := stateWithNodes(map[string]NodeState{"work": {Status: NodeStatusFailed, Attempt: 1}})
			blocked, err := Apply(st, Event{
				Type: EventNodeBlocked, Seq: 1, At: testTime, NodeID: "work", Attempt: 1,
				Reason: "retry budget exhausted", Owner: owner,
			})
			if err != nil {
				t.Fatal(err)
			}
			if blocked.Nodes["work"].Status != NodeStatusBlocked || blocked.Nodes["work"].BlockedOwner != owner {
				t.Fatalf("supported block owner was not persisted: %#v", blocked.Nodes["work"])
			}
		})
	}
}

func TestCommandIssuedOnlyStartAttemptClaimsActiveAttempt(t *testing.T) {
	st, err := ApplyAll(State{}, []Event{
		initEvent(),
		{Type: EventNodeAttemptStarted, Seq: 2, NodeID: "implement", Actor: "agent:agt_dev123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes["implement"].ActiveAttempt == nil || st.Nodes["implement"].ActiveAttempt.CommandID != "" {
		t.Fatalf("unexpected active attempt before command: %#v", st.Nodes["implement"].ActiveAttempt)
	}

	next, err := Apply(st, Event{
		Type: EventCommandIssued,
		Seq:  3,
		Command: &OutstandingCommand{
			ID:     "cmd_activate",
			NodeID: "implement",
			Kind:   CommandKindActivateNode,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Nodes["implement"].ActiveAttempt.CommandID; got != "" {
		t.Fatalf("non-start command claimed active attempt command id %q", got)
	}
}

func TestCommandIssuedCanReuseInactiveCommandSlot(t *testing.T) {
	for _, status := range []CommandStatus{CommandStatusCanceled, CommandStatusReconciled} {
		t.Run(string(status), func(t *testing.T) {
			st, err := Apply(State{}, initEvent())
			if err != nil {
				t.Fatal(err)
			}
			st.OutstandingCommands["cmd_retry"] = OutstandingCommand{
				ID:        "cmd_retry",
				NodeID:    "implement",
				Kind:      CommandKindStartAttempt,
				Status:    status,
				CreatedAt: testTime.Add(-time.Hour),
			}

			next, err := Apply(st, Event{
				Type: EventCommandIssued,
				Seq:  2,
				At:   testTime,
				Command: &OutstandingCommand{
					ID:      "cmd_retry",
					NodeID:  "implement",
					Kind:    CommandKindStartAttempt,
					Attempt: 2,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			command := next.OutstandingCommands["cmd_retry"]
			if command.Status != CommandStatusIssued || command.Attempt != 2 || command.CreatedAt != testTime {
				t.Fatalf("command = %#v", command)
			}
		})
	}
}

func TestSettleNodeStatusUsesSharedOutcomeVocabularyAndRetryBudget(t *testing.T) {
	retry := &model.RetryPolicy{MaxAttempts: 2}
	tests := []struct {
		name    string
		outcome string
		attempt int
		want    NodeStatus
	}{
		{name: "pass completes", outcome: "done", attempt: 1, want: NodeStatusCompleted},
		{name: "failure retries within budget", outcome: "cancelled", attempt: 1, want: NodeStatusReady},
		{name: "failure exhausts budget", outcome: "cancelled", attempt: 2, want: NodeStatusFailed},
		{name: "unknown outcome fails without retry", outcome: "unknown", attempt: 1, want: NodeStatusFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var policy *model.RetryPolicy
			if tt.outcome != "unknown" {
				policy = retry
			}
			if got := SettleNodeStatus(tt.outcome, tt.attempt, policy); got != tt.want {
				t.Fatalf("status = %s, want %s", got, tt.want)
			}
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
		At:     testTime,
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
			Command: &OutstandingCommand{ID: "cmd_1", NodeID: "implement", Kind: CommandKindStartAttempt},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clone := Clone(st)
	clone.Nodes["implement"].ActiveAttempt.Actor = "agent:agt_other"
	clone.Waits["wait_1"] = WaitRecord{ID: "wait_1", NodeID: "wait-human", Kind: WaitKindAgent, Status: WaitStatusCanceled}
	clone.OutstandingCommands["cmd_1"] = OutstandingCommand{ID: "cmd_1", NodeID: "implement", Kind: CommandKindCompleteRun}

	if st.Nodes["implement"].ActiveAttempt.Actor != "agent:agt_dev123" {
		t.Fatal("node active attempt aliased")
	}
	if st.Waits["wait_1"].Kind != WaitKindHuman {
		t.Fatal("wait map aliased")
	}
	if st.OutstandingCommands["cmd_1"].Kind != CommandKindStartAttempt {
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
	commandIssued, err := Apply(base, Event{
		Type: EventCommandIssued,
		Seq:  2,
		Command: &OutstandingCommand{
			ID:     "cmd_1",
			NodeID: "implement",
			Kind:   CommandKindStartAttempt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	commandObserved, err := Apply(commandIssued, Event{Type: EventCommandObserved, Seq: 3, CommandID: "cmd_1"})
	if err != nil {
		t.Fatal(err)
	}
	unsupportedBlockContact := Clone(base)
	unsupportedBlockContact.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", "implement", 1, "human:operator", CommandStatusObserved)
	unsupportedPayloadOwner := Clone(base)
	unsupportedPayloadOwner.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", "implement", 1, "agent:agt_worker", CommandStatusObserved)
	mismatchedPayloadOwner := Clone(base)
	mismatchedPayloadOwner.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", "implement", 1, "human:operator", CommandStatusObserved)

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
		{name: "node status set without status", st: base, event: Event{Type: EventNodeStatusSet, Seq: 11, NodeID: "implement"}, want: "requires nodeStatus"},
		{name: "invalid node status set", st: base, event: Event{Type: EventNodeStatusSet, Seq: 11, NodeID: "implement", NodeStatus: "bogus"}, want: "invalid node status"},
		{name: "node status set cannot forge skip decision", st: base, event: Event{Type: EventNodeStatusSet, Seq: 11, NodeID: "implement", NodeStatus: NodeStatusSkipped}, want: "cannot set status"},
		{name: "node status set cannot imply missing wait", st: base, event: Event{Type: EventNodeStatusSet, Seq: 11, NodeID: "implement", NodeStatus: NodeStatusWaitingHuman}, want: "cannot set status"},
		{name: "undeclared node", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, NodeID: "missing"}, want: "not declared"},
		{name: "start while running", st: running, event: Event{Type: EventNodeAttemptStarted, Seq: 11, NodeID: "implement", Actor: "agent:agt_dev123"}, want: "active attempt"},
		{name: "attempt regression", st: baseWithAttempt(2), event: Event{Type: EventNodeAttemptStarted, Seq: 11, NodeID: "implement", Attempt: 1, Actor: "agent:agt_dev123"}, want: "must be greater"},
		{name: "settle without outcome", st: running, event: Event{Type: EventNodeAttemptSettled, Seq: 11, NodeID: "implement"}, want: "requires outcome"},
		{name: "invalid node status override", st: running, event: Event{Type: EventNodeAttemptSettled, Seq: 11, NodeID: "implement", Outcome: "pass", NodeStatus: "bogus"}, want: "invalid node status"},
		{name: "settled status cannot imply missing wait", st: running, event: Event{Type: EventNodeAttemptSettled, Seq: 11, NodeID: "implement", Outcome: "waiting", NodeStatus: NodeStatusWaitingHuman}, want: "cannot set status"},
		{name: "second decision", st: decisionDone, event: Event{Type: EventDecisionRecorded, Seq: 11, NodeID: "decide", ChosenEdge: "reject", Decision: &DecisionRecord{Actor: "human:johan", Verdict: "reject"}}, want: "already decided"},
		{name: "redecision after unblock", st: unblockedDecision(t, decisionDone), event: Event{Type: EventDecisionRecorded, Seq: 12, NodeID: "decide", ChosenEdge: "reject", Decision: &DecisionRecord{Actor: "human:johan", Verdict: "reject"}}, want: "already decided"},
		{name: "decision chosen edge verdict mismatch", st: base, event: Event{Type: EventDecisionRecorded, Seq: 11, NodeID: "decide", ChosenEdge: "approve", Decision: &DecisionRecord{Actor: "human:johan", Verdict: "reject"}}, want: "must match verdict"},
		{name: "block without reason", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, NodeID: "implement", Owner: "human:johan"}, want: "requires reason and owner"},
		{name: "block without owner", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, NodeID: "implement", Reason: "blocked"}, want: "requires reason and owner"},
		{name: "block without timestamp", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, NodeID: "implement", Reason: "blocked", Owner: "human:johan"}, want: "requires timestamp"},
		{name: "block with agent owner", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, At: testTime, NodeID: "implement", Reason: "blocked", Owner: "agent:agt_worker"}, want: "requires a human/role owner"},
		{name: "block with program owner", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, At: testTime, NodeID: "implement", Reason: "blocked", Owner: "program:deploy"}, want: "requires a human/role owner"},
		{name: "block with system owner", st: base, event: Event{Type: EventNodeBlocked, Seq: 11, At: testTime, NodeID: "implement", Reason: "blocked", Owner: "system:deploy"}, want: "requires a human/role owner"},
		{name: "block contact with unsupported owner", st: unsupportedBlockContact, event: Event{Type: EventContactScheduled, Seq: 11, Contact: &ContactState{CommandID: "cmd_block", Kind: WaitKindAgent, Assignee: "agent:agt_worker", Cadence: "5m0s", Budget: 3, EscalationTarget: "human:operator"}}, want: "requires a human/role owner"},
		{name: "block contact with unsupported payload owner", st: unsupportedPayloadOwner, event: Event{Type: EventContactScheduled, Seq: 11, Contact: &ContactState{CommandID: "cmd_block", Kind: WaitKindHuman, Assignee: "human:operator", Cadence: "30m0s", Budget: 5, EscalationTarget: "human:operator"}}, want: "payload owner \"agent:agt_worker\" is unsupported"},
		{name: "block contact assignee mismatches payload owner", st: mismatchedPayloadOwner, event: Event{Type: EventContactScheduled, Seq: 11, Contact: &ContactState{CommandID: "cmd_block", Kind: WaitKindHuman, Assignee: "human:someone-else", Cadence: "30m0s", Budget: 5, EscalationTarget: "human:operator"}}, want: "does not match payload owner \"human:operator\""},
		{name: "block resolution without timestamp", st: base, event: Event{Type: EventBlockResolutionRecorded, Seq: 11, Resolution: &BlockResolution{NodeID: "implement", BlockedAttempt: 1, Decision: BlockDecisionRetry, Actor: "human:johan", Reason: "retry", EvidenceRef: "decision:retry"}}, want: "requires timestamp"},
		{name: "unknown wait", st: base, event: Event{Type: EventWaitSatisfied, Seq: 11, WaitID: "missing"}, want: "not declared"},
		{name: "invalid wait kind", st: base, event: Event{Type: EventWaitCreated, Seq: 11, Wait: &WaitRecord{ID: "wait", NodeID: "wait-human", Kind: "bogus"}}, want: "invalid wait kind"},
		{name: "invalid wait status", st: base, event: Event{Type: EventWaitCreated, Seq: 11, Wait: &WaitRecord{ID: "wait", NodeID: "wait-human", Kind: WaitKindHuman, Status: "bogus"}}, want: "invalid wait status"},
		{name: "unknown timer", st: base, event: Event{Type: EventTimerSatisfied, Seq: 11, TimerID: "missing"}, want: "not declared"},
		{name: "command without id", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{NodeID: "implement", Kind: CommandKindStartAttempt}}, want: "command id"},
		{name: "invalid command kind", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{ID: "cmd", NodeID: "implement", Kind: "agent.spawn"}}, want: "invalid command kind"},
		{name: "invalid command status", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{ID: "cmd", NodeID: "implement", Kind: CommandKindStartAttempt, Status: "bogus"}}, want: "invalid command status"},
		{name: "command issued observed status", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{ID: "cmd", NodeID: "implement", Kind: CommandKindStartAttempt, Status: CommandStatusObserved}}, want: "requires issued status"},
		{name: "command negative attempt", st: base, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{ID: "cmd", NodeID: "implement", Kind: CommandKindStartAttempt, Attempt: -1}}, want: "non-negative"},
		{name: "duplicate command issued", st: commandIssued, event: Event{Type: EventCommandIssued, Seq: 11, Command: &OutstandingCommand{ID: "cmd_1", NodeID: "implement", Kind: CommandKindStartAttempt}}, want: "already outstanding"},
		{name: "unknown command observed", st: base, event: Event{Type: EventCommandObserved, Seq: 11, CommandID: "missing"}, want: "not outstanding"},
		{name: "observed command observed again", st: commandObserved, event: Event{Type: EventCommandObserved, Seq: 11, CommandID: "cmd_1"}, want: "cannot be observed"},
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
			name: "invalid run status",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.Status = "bogus_run_status"
				return st
			}(),
			code: "invalid_run_status",
		},
		{
			name: "invalid node status",
			st: stateWithNodes(map[string]NodeState{
				"a": {Status: "bogus_node_status"},
			}),
			code: "invalid_node_status",
		},
		{
			name: "invalid node type",
			st: stateWithNodes(map[string]NodeState{
				"a": {Type: "bogus_node_type", Status: NodeStatusPending},
			}),
			code: "invalid_node_type",
		},
		{
			name: "invalid command status",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.OutstandingCommands["cmd"] = OutstandingCommand{ID: "cmd", NodeID: "a", Kind: CommandKindStartAttempt, Status: "bogus_command_status"}
				return st
			}(),
			code: "invalid_command_status",
		},
		{
			name: "invalid command kind",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.OutstandingCommands["cmd"] = OutstandingCommand{ID: "cmd", NodeID: "a", Kind: "agent.spawn", Status: CommandStatusIssued}
				return st
			}(),
			code: "invalid_command_kind",
		},
		{
			name: "command id mismatch",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.OutstandingCommands["cmd"] = OutstandingCommand{ID: "other", NodeID: "a", Kind: CommandKindStartAttempt, Status: CommandStatusIssued}
				return st
			}(),
			code: "command_id_key_mismatch",
		},
		{
			name: "command unknown node",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.OutstandingCommands["cmd"] = OutstandingCommand{ID: "cmd", NodeID: "missing", Kind: CommandKindStartAttempt, Status: CommandStatusIssued}
				return st
			}(),
			code: "command_unknown_node",
		},
		{
			name: "invalid wait kind",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.Waits["wait"] = WaitRecord{ID: "wait", NodeID: "a", Kind: "bogus_wait_kind", Status: WaitStatusPending}
				return st
			}(),
			code: "invalid_wait_kind",
		},
		{
			name: "invalid wait status",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.Waits["wait"] = WaitRecord{ID: "wait", NodeID: "a", Kind: WaitKindHuman, Status: "bogus_wait_status"}
				return st
			}(),
			code: "invalid_wait_status",
		},
		{
			name: "invalid timer status",
			st: func() State {
				st := stateWithNodes(map[string]NodeState{"a": {Status: NodeStatusPending}})
				st.Timers["timer"] = TimerRecord{ID: "timer", NodeID: "a", Status: "bogus_timer_status"}
				return st
			}(),
			code: "invalid_timer_status",
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
			name: "skipped node without block resolution",
			st: stateWithNodes(map[string]NodeState{
				"a": {Status: NodeStatusSkipped},
			}),
			code: "skipped_node_without_block_resolution",
		},
		{
			name: "cancel resolution without canceled run",
			st: func() State {
				resolution := &BlockResolution{
					NodeID: "a", BlockedAttempt: 1, Decision: BlockDecisionCancel,
					Actor: "human:johan", Reason: "operator canceled", EvidenceRef: "decision:cancel", Timestamp: testTime,
				}
				st := stateWithNodes(map[string]NodeState{
					"a": {Status: NodeStatusSkipped, BlockedAttempt: 1, BlockedNodeID: "a", BlockResolution: resolution},
				})
				st.AdminRecords = []AdminRecord{{
					Type: EventBlockResolutionRecorded, Actor: resolution.Actor, Reason: resolution.Reason,
					EvidenceRef: resolution.EvidenceRef, Timestamp: resolution.Timestamp, Resolution: resolution,
				}}
				return st
			}(),
			code: "cancel_resolution_without_canceled_run",
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
			Status:         NodeStatusBlocked,
			BlockedReason:  "needs review",
			BlockedOwner:   "human:johan",
			BlockedAt:      testTime,
			BlockedAttempt: 1,
			BlockedNodeID:  "blocked",
		},
	})
	st.Waits["wait_1"] = WaitRecord{ID: "wait_1", NodeID: "wait", Kind: WaitKindHuman, Status: WaitStatusPending}
	st.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", "blocked", 1, "human:johan", CommandStatusObserved)
	st.Contacts = map[string]ContactState{"cmd_block": {
		CommandID: "cmd_block", Kind: WaitKindHuman, Assignee: "human:johan", Cadence: "30m0s", Budget: 5,
		EscalationTarget: "human:operator", NextContactAt: testTime.Add(30 * time.Minute),
	}}

	if diagnostics := CheckInvariants(&st); diagnostics.HasErrors() {
		t.Fatalf("unexpected invariant errors: %#v", diagnostics)
	}

	mismatched := Clone(st)
	contact := mismatched.Contacts["cmd_block"]
	contact.Assignee = "human:someone-else"
	mismatched.Contacts["cmd_block"] = contact
	if diagnostics := CheckInvariants(&mismatched); !hasDiagnostic(diagnostics, "blocked_contact_owner_mismatch") {
		t.Fatalf("mismatched blocked contact owner diagnostics = %#v", diagnostics)
	}

	closed := Clone(st)
	command := closed.OutstandingCommands["cmd_block"]
	command.Status = CommandStatusCanceled
	closed.OutstandingCommands["cmd_block"] = command
	if diagnostics := CheckInvariants(&closed); !hasDiagnostic(diagnostics, "blocked_node_contact_count") {
		t.Fatalf("closed blocked contact command diagnostics = %#v", diagnostics)
	}

	unsupportedPayload := Clone(st)
	unsupportedPayload.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", "blocked", 1, "agent:agt_worker", CommandStatusObserved)
	if diagnostics := CheckInvariants(&unsupportedPayload); !hasDiagnostic(diagnostics, "unsupported_blocked_contact_payload_owner") {
		t.Fatalf("unsupported blocked command payload owner diagnostics = %#v", diagnostics)
	}

	roleOwned := Clone(st)
	roleNode := roleOwned.Nodes["blocked"]
	roleNode.BlockedOwner = "role:oncall"
	roleOwned.Nodes["blocked"] = roleNode
	roleOwned.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", "blocked", 1, "role:oncall", CommandStatusObserved)
	roleContact := roleOwned.Contacts["cmd_block"]
	roleContact.Assignee = "role:oncall"
	roleOwned.Contacts["cmd_block"] = roleContact
	if diagnostics := CheckInvariants(&roleOwned); diagnostics.HasErrors() {
		t.Fatalf("role-owned blocked contact must verify: %#v", diagnostics)
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

func TestBlockedStateSchemaCompatibility(t *testing.T) {
	legacy := []byte(`{"stateSchemaVersion":5,"runId":"legacy","status":"running","originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{"work":{"status":"blocked","attempt":1,"blockedReason":"legacy poison","blockedOwner":"human:operator","blockedAttempt":1,"blockedNodeId":"work"}},"lastLogSeq":3,"logChecksum":""}`)
	st, err := Decode(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if st.StateSchemaVersion != 5 || !st.Nodes["work"].BlockedAt.IsZero() {
		t.Fatalf("legacy decode = %#v", st.Nodes["work"])
	}
	if diagnostics := CheckInvariants(st); diagnostics.HasErrors() {
		t.Fatalf("legacy v5 block must remain verifiable: %#v", diagnostics)
	}

	current := strings.Replace(string(legacy), `"stateSchemaVersion":5`, `"stateSchemaVersion":6`, 1)
	v6, err := Decode([]byte(current))
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := CheckInvariants(v6)
	if !hasDiagnostic(diagnostics, "blocked_node_without_timestamp") || !hasDiagnostic(diagnostics, "blocked_node_contact_count") {
		t.Fatalf("malformed v6 block diagnostics = %#v", diagnostics)
	}
}

func TestLegacyBlockedStateCanUpdateContactAndResolveWithoutSchemaPromotion(t *testing.T) {
	legacy := []byte(`{"stateSchemaVersion":5,"runId":"legacy","status":"running","originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{"work":{"status":"blocked","attempt":1,"blockedReason":"legacy poison","blockedOwner":"human:operator","blockedAttempt":1,"blockedNodeId":"work"}},"lastLogSeq":3,"logChecksum":""}`)
	st, err := Decode(legacy)
	if err != nil {
		t.Fatal(err)
	}
	st.OutstandingCommands["cmd_legacy_block"] = *blockCommandForTest("cmd_legacy_block", "work", 1, "human:operator", CommandStatusObserved)
	updated, err := Apply(*st, Event{Type: EventContactScheduled, Seq: 4, At: testTime, Contact: &ContactState{
		CommandID: "cmd_legacy_block", Kind: WaitKindHuman, Assignee: "human:operator",
		Cadence: "30m0s", Budget: 5, EscalationTarget: "human:operator", NextContactAt: testTime.Add(30 * time.Minute),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.StateSchemaVersion != 5 {
		t.Fatalf("legacy contact update promoted schema to %d", updated.StateSchemaVersion)
	}
	resolution := BlockResolution{
		NodeID: "work", BlockedAttempt: 1, Decision: BlockDecisionRetry,
		Actor: "human:operator", Reason: "legacy block cleared", EvidenceRef: "decision:legacy", Timestamp: testTime.Add(time.Hour),
	}
	resolved, err := ApplyAll(updated, []Event{
		{Type: EventBlockResolutionRecorded, Seq: 5, At: resolution.Timestamp, Resolution: &resolution},
		{Type: EventNodeUnblocked, Seq: 6, At: resolution.Timestamp, NodeID: "work", NodeStatus: NodeStatusReady, Resolution: &resolution},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.StateSchemaVersion != 5 || !resolved.Nodes["work"].BlockedAt.IsZero() {
		t.Fatalf("legacy resolution fabricated v6 state: version=%d node=%#v", resolved.StateSchemaVersion, resolved.Nodes["work"])
	}
	contact := resolved.Contacts["cmd_legacy_block"]
	if !contact.Paused || !contact.NextContactAt.IsZero() {
		t.Fatalf("legacy block contact remained active: %#v", contact)
	}
	if diagnostics := CheckInvariants(&resolved); diagnostics.HasErrors() {
		t.Fatalf("resolved legacy block must remain verifiable: %#v", diagnostics)
	}

	resolved.Nodes["fresh"] = NodeState{Status: NodeStatusFailed, Attempt: 1}
	resolved.OutstandingCommands["cmd_fresh_block"] = *blockCommandForTest("cmd_fresh_block", "fresh", 1, "human:operator", CommandStatusObserved)
	mixed, err := ApplyAll(resolved, []Event{
		{Type: EventContactScheduled, Seq: 7, At: resolution.Timestamp, Contact: &ContactState{
			CommandID: "cmd_fresh_block", Kind: WaitKindHuman, Assignee: "human:operator",
			Cadence: "30m0s", Budget: 5, EscalationTarget: "human:operator", NextContactAt: resolution.Timestamp.Add(30 * time.Minute),
		}},
		{Type: EventNodeBlocked, Seq: 8, At: resolution.Timestamp, NodeID: "fresh", Attempt: 1, Reason: "fresh poison", Owner: "human:operator"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.StateSchemaVersion != StateSchemaVersion || !mixed.Nodes["work"].BlockedAtUnavailable {
		t.Fatalf("mixed-generation promotion lost legacy provenance: version=%d legacy=%#v", mixed.StateSchemaVersion, mixed.Nodes["work"])
	}
	if mixed.Nodes["fresh"].BlockedAtUnavailable || mixed.Nodes["fresh"].BlockedAt.IsZero() {
		t.Fatalf("fresh block was marked legacy: %#v", mixed.Nodes["fresh"])
	}
	if diagnostics := CheckInvariants(&mixed); diagnostics.HasErrors() {
		t.Fatalf("mixed legacy and v6 block generations must verify: %#v", diagnostics)
	}
}

func TestEnginePauseAndResumeAreDurableReducerState(t *testing.T) {
	st := stateWithNodes(map[string]NodeState{"work": {Type: model.NodeTypeTask, Status: NodeStatusRunning, ActiveAttempt: &AttemptState{Attempt: 1, CommandID: "cmd_work"}}})
	st.RunID = "run_pause"
	st.OutstandingCommands["cmd_work"] = OutstandingCommand{ID: "cmd_work", NodeID: "work", Attempt: 1, Kind: CommandKindStartAttempt, Status: CommandStatusIssued}
	until := time.Date(2026, 7, 9, 22, 0, 0, 0, time.UTC)
	paused, err := Apply(st, Event{Type: EventRunPaused, Pause: &PauseState{
		Kind: PauseKindRateLimited, Reason: "rate limited until 22:00", CommandID: "cmd_work", Until: until,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != RunStatusPaused || paused.Pause == nil || !paused.Pause.Until.Equal(until) {
		t.Fatalf("paused state = %#v", paused)
	}
	if diagnostics := CheckInvariants(&paused); diagnostics.HasErrors() {
		t.Fatalf("pause invariants = %#v", diagnostics)
	}
	body, err := Encode(&paused)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Pause == nil || decoded.Pause.CommandID != "cmd_work" {
		t.Fatalf("decoded pause = %#v", decoded.Pause)
	}
	resumed, err := Apply(*decoded, Event{Type: EventRunResumed})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != RunStatusRunning || resumed.Pause != nil {
		t.Fatalf("resumed state = %#v", resumed)
	}
	repaired, err := Apply(paused, Event{Type: EventAdminRepairRecorded, RunStatus: RunStatusRunning, Actor: "human:test", Reason: "reviewed pause"})
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Status != RunStatusRunning || repaired.Pause != nil {
		t.Fatalf("repaired state retained pause = %#v", repaired)
	}
}

func TestTimestampLessLegacyAdminReplayInventory(t *testing.T) {
	base := New("run", "demo@sha256:x", "demo@sha256:x", nil)
	for _, eventType := range []EventType{EventAdminRepairRecorded, EventAdminProgramsAllowed} {
		t.Run(string(eventType), func(t *testing.T) {
			replayed, err := ApplyAll(base, []Event{{
				Type: eventType, Seq: 1, Actor: "human:operator", Reason: "historical audit",
			}})
			if err != nil {
				t.Fatalf("historical replay failed: %v", err)
			}
			if len(replayed.AdminRecords) != 1 || replayed.AdminRecords[0].Type != eventType || !replayed.AdminRecords[0].Timestamp.IsZero() {
				t.Fatalf("replayed admin records = %#v", replayed.AdminRecords)
			}
		})
	}

	_, err := ApplyAll(base, []Event{{
		Type: EventBlockResolutionRecorded, Seq: 1,
		Resolution: &BlockResolution{
			NodeID: "work", BlockedAttempt: 1, Decision: BlockDecisionSkip,
			Actor: "human:operator", Reason: "waived", EvidenceRef: "ticket:TCL-523",
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "requires timestamp") {
		t.Fatalf("timestamp-less block resolution error = %v", err)
	}
}

func TestEnginePauseValidation(t *testing.T) {
	st := stateWithNodes(map[string]NodeState{"work": {Type: model.NodeTypeTask, Status: NodeStatusRunning}})
	for name, pause := range map[string]PauseState{
		"rate limit without deadline": {Kind: PauseKindRateLimited, Reason: "quota", CommandID: "cmd"},
		"reconcile without owner":     {Kind: PauseKindNeedsReconcile, Reason: "lost result", CommandID: "cmd"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Apply(st, Event{Type: EventRunPaused, Pause: &pause}); err == nil {
				t.Fatal("expected pause validation error")
			}
		})
	}
	valid := PauseState{Kind: PauseKindNeedsReconcile, Reason: "lost result", CommandID: "missing", Owner: "human:test"}
	if _, err := Apply(st, Event{Type: EventRunPaused, Pause: &valid}); err == nil || !strings.Contains(err.Error(), "is not outstanding") {
		t.Fatalf("unknown pause command error = %v", err)
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

func unblockedDecision(t *testing.T, st State) State {
	t.Helper()
	blocked, err := Apply(st, Event{Type: EventNodeBlocked, Seq: 10, At: testTime, NodeID: "decide", Reason: "recheck", Owner: "human:johan"})
	if err != nil {
		t.Fatal(err)
	}
	resolution := &BlockResolution{NodeID: "decide", BlockedAttempt: 1, Decision: BlockDecisionRetry, Actor: "human:johan", Reason: "recheck", EvidenceRef: "decision:recheck", Timestamp: testTime}
	unblocked, err := Apply(blocked, Event{Type: EventNodeUnblocked, Seq: 11, At: testTime, NodeID: "decide", NodeStatus: NodeStatusReady, Resolution: resolution})
	if err != nil {
		t.Fatal(err)
	}
	return unblocked
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

func TestPoisonEscalationDecisionRequiresEvidence(t *testing.T) {
	st := New("run", "demo@sha256:x", "demo@sha256:x", []NodeInit{{ID: "escalate", Type: model.NodeTypeDecision, Status: NodeStatusReady}})
	node := st.Nodes["escalate"]
	node.Attempt = 2
	node.PoisonedNodeID = "implement.test.tests"
	st.Nodes["escalate"] = node
	_, err := Apply(st, Event{
		Type: EventDecisionRecorded, Seq: 1, NodeID: "escalate", ChosenEdge: "retry",
		Decision: &DecisionRecord{Actor: "human:johan", Verdict: "retry", Timestamp: testTime},
	})
	if err == nil || !strings.Contains(err.Error(), "requires an evidence reference") {
		t.Fatalf("evidence-less poison decision was accepted: %v", err)
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
