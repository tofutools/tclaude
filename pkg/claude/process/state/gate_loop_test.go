package state

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const (
	workHash1 = "1111111111111111111111111111111111111111111111111111111111111111"
	workHash2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

// gateLoopEvents drives an expanded compound node through plan and do so the
// tests gate is ready, with the do evidence hash recorded. Seq numbers start
// at 1 and the caller continues from the returned next seq.
func gateLoopEvents() ([]Event, int64) {
	events := []Event{
		{
			Type:                EventRunInitialized,
			Seq:                 1,
			RunID:               "run_1",
			OriginalTemplateRef: "demo@sha256:old",
			CurrentTemplateRef:  "demo@sha256:old",
			Nodes: []NodeInit{
				{ID: "implement", Type: model.NodeTypeTask},
				{ID: "end", Type: model.NodeTypeEnd},
			},
		},
		{
			Type: EventNodeStatusSet, Seq: 2, NodeID: "implement", NodeStatus: NodeStatusReady,
		},
		{
			Type: EventNodeExpanded, Seq: 3, NodeID: "implement",
			Nodes: []NodeInit{
				{ID: "implement.plan", Parent: "implement", Stage: model.StagePlan},
				{ID: "implement.do", Parent: "implement", Stage: model.StageDo},
				{ID: "implement.test.tests", Parent: "implement", Stage: model.StageTest, StepID: "tests"},
				{ID: "implement.review", Parent: "implement", Stage: model.StageReview},
				{ID: "implement.done", Parent: "implement", Stage: model.StageDone},
			},
		},
		{Type: EventNodeAttemptStarted, Seq: 4, NodeID: "implement.plan", Actor: "agent:agt_dev123"},
		{Type: EventNodeAttemptSettled, Seq: 5, NodeID: "implement.plan", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "artifacts/plan.md"},
		{Type: EventNodeStatusSet, Seq: 6, NodeID: "implement.do", NodeStatus: NodeStatusReady},
		{Type: EventNodeAttemptStarted, Seq: 7, NodeID: "implement.do", Actor: "agent:agt_dev123"},
		{Type: EventNodeAttemptSettled, Seq: 8, NodeID: "implement.do", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "commit:abc", EvidenceHash: workHash1},
		{Type: EventNodeStatusSet, Seq: 9, NodeID: "implement.test.tests", NodeStatus: NodeStatusReady},
	}
	return events, 10
}

func applyAll(t *testing.T, events []Event) State {
	t.Helper()
	st, err := ApplyAll(State{}, events)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestGateFailFeedbackReentryAndPass(t *testing.T) {
	events, seq := gateLoopEvents()
	events = append(events,
		Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit1"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 1, NodeID: "implement.test.tests", Outcome: "fail", NodeStatus: NodeStatusFailed, EvidenceRef: "artifact:test-log", Feedback: "TestFoo fails", WorkEvidenceHash: workHash1},
	)
	st := applyAll(t, events)
	gate := st.Nodes["implement.test.tests"]
	if gate.FailCount != 1 || gate.LastEvidenceHash != workHash1 {
		t.Fatalf("gate accounting = %#v", gate)
	}
	if len(gate.Decisions) != 1 || gate.Decisions[0].Verdict != "fail" || gate.Decisions[0].Actor != "program:go test@exit1" {
		t.Fatalf("gate decisions = %#v", gate.Decisions)
	}

	// The feedback batch: payload to do, span reset, do re-readied.
	events = append(events,
		Event{Type: EventFeedbackRecorded, Seq: seq + 2, NodeID: "implement.do", FromNodeID: "implement.test.tests", Feedback: "TestFoo fails", EvidenceRef: "artifact:test-log"},
		Event{Type: EventGateLoopReset, Seq: seq + 3, NodeID: "implement", Gates: []string{"implement.test.tests"}, Reason: "tests failed"},
		Event{Type: EventNodeStatusSet, Seq: seq + 4, NodeID: "implement.do", NodeStatus: NodeStatusReady},
	)
	st = applyAll(t, events)
	work := st.Nodes["implement.do"]
	if work.Status != NodeStatusReady || work.PendingFeedback == nil {
		t.Fatalf("work after feedback = %#v", work)
	}
	if work.PendingFeedback.FromNodeID != "implement.test.tests" || work.PendingFeedback.Feedback != "TestFoo fails" {
		t.Fatalf("pending feedback = %#v", work.PendingFeedback)
	}
	if st.Nodes["implement.test.tests"].Status != NodeStatusPending {
		t.Fatalf("gate after reset = %#v", st.Nodes["implement.test.tests"])
	}
	// Counter persists across a same-kind reset: the gate stays in its window.
	if st.Nodes["implement.test.tests"].FailCount != 1 {
		t.Fatalf("gate fail count = %d", st.Nodes["implement.test.tests"].FailCount)
	}

	// The next do attempt consumes the pending feedback marker.
	events = append(events,
		Event{Type: EventNodeAttemptStarted, Seq: seq + 5, NodeID: "implement.do", Actor: "agent:agt_dev123"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 6, NodeID: "implement.do", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "commit:def", EvidenceHash: workHash2},
		Event{Type: EventNodeStatusSet, Seq: seq + 7, NodeID: "implement.test.tests", NodeStatus: NodeStatusReady},
		Event{Type: EventNodeAttemptStarted, Seq: seq + 8, NodeID: "implement.test.tests", Actor: "program:go test@exit0"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 9, NodeID: "implement.test.tests", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "artifact:test-log-2", WorkEvidenceHash: workHash2},
	)
	st = applyAll(t, events)
	if st.Nodes["implement.do"].PendingFeedback != nil {
		t.Fatalf("pending feedback must be consumed on attempt start: %#v", st.Nodes["implement.do"].PendingFeedback)
	}
	gate = st.Nodes["implement.test.tests"]
	if gate.Status != NodeStatusCompleted || gate.FailCount != 1 || gate.LastEvidenceHash != workHash2 {
		t.Fatalf("gate after pass = %#v", gate)
	}
	if len(gate.Decisions) != 2 || gate.Decisions[1].Verdict != "pass" {
		t.Fatalf("gate decisions = %#v", gate.Decisions)
	}
	if diags := CheckInvariants(&st); diags.HasErrors() {
		t.Fatalf("invariants: %#v", diags.Errors())
	}
}

func TestHashlessGateSettleClearsLastEvidenceHash(t *testing.T) {
	// A settle without a work-evidence hash must CLEAR the recorded hash, not
	// preserve the previous window's: otherwise a later re-entry could
	// short-circuit against a verdict that evaluated different, unhashed work
	// (e.g. window 3 reverts to window 1's bytes while window 2 settled with
	// no --evidence-hash).
	events, seq := gateLoopEvents()
	events = append(events,
		Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit1"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 1, NodeID: "implement.test.tests", Outcome: "fail", NodeStatus: NodeStatusFailed, EvidenceRef: "artifact:log-1", WorkEvidenceHash: workHash1},
		Event{Type: EventFeedbackRecorded, Seq: seq + 2, NodeID: "implement.do", FromNodeID: "implement.test.tests", Feedback: "fails"},
		Event{Type: EventGateLoopReset, Seq: seq + 3, NodeID: "implement", Gates: []string{"implement.test.tests"}, Reason: "tests failed"},
		Event{Type: EventNodeStatusSet, Seq: seq + 4, NodeID: "implement.do", NodeStatus: NodeStatusReady},
		// The do stage re-runs and settles WITHOUT an evidence hash.
		Event{Type: EventNodeAttemptStarted, Seq: seq + 5, NodeID: "implement.do", Actor: "agent:agt_dev123"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 6, NodeID: "implement.do", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "commit:def"},
		Event{Type: EventNodeStatusSet, Seq: seq + 7, NodeID: "implement.test.tests", NodeStatus: NodeStatusReady},
		Event{Type: EventNodeAttemptStarted, Seq: seq + 8, NodeID: "implement.test.tests", Actor: "program:go test@exit1"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 9, NodeID: "implement.test.tests", Outcome: "fail", NodeStatus: NodeStatusFailed, EvidenceRef: "artifact:log-2"},
	)
	st := applyAll(t, events)
	gate := st.Nodes["implement.test.tests"]
	if gate.LastEvidenceHash != "" {
		t.Fatalf("hashless settle must clear the recorded hash, got %q", gate.LastEvidenceHash)
	}
}

func TestGateLoopResetZeroesCrossKindCounters(t *testing.T) {
	events, seq := gateLoopEvents()
	events = append(events,
		Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit0"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 1, NodeID: "implement.test.tests", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "artifact:test-log", WorkEvidenceHash: workHash1},
		Event{Type: EventNodeStatusSet, Seq: seq + 2, NodeID: "implement.review", NodeStatus: NodeStatusReady},
		Event{Type: EventNodeAttemptStarted, Seq: seq + 3, NodeID: "implement.review", Actor: "agent:agt_reviewer"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 4, NodeID: "implement.review", Outcome: "fail", NodeStatus: NodeStatusFailed, EvidenceRef: "artifact:review", Feedback: "missing edge case", WorkEvidenceHash: workHash1},
	)
	// Seed a nonzero tests counter by tampering pre-reset state is not
	// possible through events alone (the tests gate passed), so reset both
	// gates and assert the cross-kind counter zeroing on the review-fail
	// span: tests is a different stage kind than review.
	events = append(events,
		Event{Type: EventFeedbackRecorded, Seq: seq + 5, NodeID: "implement.do", FromNodeID: "implement.review", Feedback: "missing edge case"},
		Event{Type: EventGateLoopReset, Seq: seq + 6, NodeID: "implement", Gates: []string{"implement.test.tests", "implement.review"}, ResetCounters: []string{"implement.test.tests"}, Reason: "review failed"},
		Event{Type: EventNodeStatusSet, Seq: seq + 7, NodeID: "implement.do", NodeStatus: NodeStatusReady},
	)
	st := applyAll(t, events)
	tests := st.Nodes["implement.test.tests"]
	review := st.Nodes["implement.review"]
	if tests.Status != NodeStatusPending || review.Status != NodeStatusPending {
		t.Fatalf("span after reset: tests %s review %s", tests.Status, review.Status)
	}
	if tests.FailCount != 0 {
		t.Fatalf("tests counter must reset, got %d", tests.FailCount)
	}
	if review.FailCount != 1 {
		t.Fatalf("review counter must persist, got %d", review.FailCount)
	}
	if diags := CheckInvariants(&st); diags.HasErrors() {
		t.Fatalf("invariants: %#v", diags.Errors())
	}
}

func TestGateShortCircuitPassAndFail(t *testing.T) {
	events, seq := gateLoopEvents()
	events = append(events,
		Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit0"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 1, NodeID: "implement.test.tests", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "artifact:test-log", WorkEvidenceHash: workHash1},
		Event{Type: EventGateLoopReset, Seq: seq + 2, NodeID: "implement", Gates: []string{"implement.test.tests"}, Reason: "review failed"},
		Event{Type: EventNodeStatusSet, Seq: seq + 3, NodeID: "implement.test.tests", NodeStatus: NodeStatusReady},
		Event{Type: EventGateShortCircuited, Seq: seq + 4, NodeID: "implement.test.tests", Actor: ActorEvidenceUnchanged, EvidenceHash: workHash1},
	)
	st := applyAll(t, events)
	gate := st.Nodes["implement.test.tests"]
	if gate.Status != NodeStatusCompleted || gate.Attempt != 1 {
		t.Fatalf("short-circuited gate = %#v", gate)
	}
	if len(gate.Decisions) != 2 {
		t.Fatalf("decisions = %#v", gate.Decisions)
	}
	engineDecision := gate.Decisions[1]
	if engineDecision.Actor != ActorEvidenceUnchanged || engineDecision.Verdict != "pass" || engineDecision.EvidenceRef != gate.Decisions[0].EvidenceRef {
		t.Fatalf("engine decision = %#v", engineDecision)
	}
	if diags := CheckInvariants(&st); diags.HasErrors() {
		t.Fatalf("invariants: %#v", diags.Errors())
	}
}

func TestGateShortCircuitFailIncrementsCounter(t *testing.T) {
	events, seq := gateLoopEvents()
	events = append(events,
		Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit1"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 1, NodeID: "implement.test.tests", Outcome: "fail", NodeStatus: NodeStatusFailed, EvidenceRef: "artifact:test-log", WorkEvidenceHash: workHash1},
		Event{Type: EventFeedbackRecorded, Seq: seq + 2, NodeID: "implement.do", FromNodeID: "implement.test.tests", Feedback: "fails"},
		Event{Type: EventGateLoopReset, Seq: seq + 3, NodeID: "implement", Gates: []string{"implement.test.tests"}, Reason: "tests failed"},
		Event{Type: EventNodeStatusSet, Seq: seq + 4, NodeID: "implement.do", NodeStatus: NodeStatusReady},
		// The do stage re-ran but produced identical evidence.
		Event{Type: EventNodeAttemptStarted, Seq: seq + 5, NodeID: "implement.do", Actor: "agent:agt_dev123"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 6, NodeID: "implement.do", Outcome: "pass", NodeStatus: NodeStatusCompleted, EvidenceRef: "commit:abc", EvidenceHash: workHash1},
		Event{Type: EventNodeStatusSet, Seq: seq + 7, NodeID: "implement.test.tests", NodeStatus: NodeStatusReady},
		Event{Type: EventGateShortCircuited, Seq: seq + 8, NodeID: "implement.test.tests", Actor: ActorEvidenceUnchanged, EvidenceHash: workHash1},
	)
	st := applyAll(t, events)
	gate := st.Nodes["implement.test.tests"]
	if gate.Status != NodeStatusFailed || gate.FailCount != 2 {
		t.Fatalf("failed short-circuit = %#v", gate)
	}
	if len(gate.Decisions) != 2 || gate.Decisions[1].Verdict != "fail" || gate.Decisions[1].Actor != ActorEvidenceUnchanged {
		t.Fatalf("decisions = %#v", gate.Decisions)
	}
}

func TestGateLoopReducerErrors(t *testing.T) {
	base, seq := gateLoopEvents()
	failedGate := append(append([]Event(nil), base...),
		Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit1"},
		Event{Type: EventNodeAttemptSettled, Seq: seq + 1, NodeID: "implement.test.tests", Outcome: "fail", NodeStatus: NodeStatusFailed, EvidenceRef: "artifact:test-log", WorkEvidenceHash: workHash1},
	)
	tests := []struct {
		name    string
		events  []Event
		bad     Event
		wantErr string
	}{
		{
			name:    "feedback target must be plan or do",
			events:  failedGate,
			bad:     Event{Type: EventFeedbackRecorded, NodeID: "implement.review", FromNodeID: "implement.test.tests", Feedback: "x"},
			wantErr: "must be a plan or do stage child",
		},
		{
			name:    "feedback source must be a sibling gate",
			events:  failedGate,
			bad:     Event{Type: EventFeedbackRecorded, NodeID: "implement.do", FromNodeID: "implement.plan", Feedback: "x"},
			wantErr: "must be a sibling gate",
		},
		{
			name:    "loop reset rejects running gates",
			events:  append(append([]Event(nil), base...), Event{Type: EventNodeAttemptStarted, Seq: seq, NodeID: "implement.test.tests", Actor: "program:go test@exit1"}),
			bad:     Event{Type: EventGateLoopReset, NodeID: "implement", Gates: []string{"implement.test.tests"}},
			wantErr: "only settled or pending gates re-enter",
		},
		{
			name:    "loop reset rejects non-gate children",
			events:  failedGate,
			bad:     Event{Type: EventGateLoopReset, NodeID: "implement", Gates: []string{"implement.do"}},
			wantErr: "is not a gate child",
		},
		{
			name:    "short-circuit requires matching evidence hash",
			events:  append(append([]Event(nil), failedGate...), Event{Type: EventGateLoopReset, Seq: seq + 2, NodeID: "implement", Gates: []string{"implement.test.tests"}}, Event{Type: EventNodeStatusSet, Seq: seq + 3, NodeID: "implement.do", NodeStatus: NodeStatusReady}),
			bad:     Event{Type: EventGateShortCircuited, NodeID: "implement.test.tests", Actor: ActorEvidenceUnchanged, EvidenceHash: workHash2},
			wantErr: "evidence hash does not match",
		},
		{
			name:    "short-circuit requires engine actor",
			events:  append(append([]Event(nil), failedGate...), Event{Type: EventGateLoopReset, Seq: seq + 2, NodeID: "implement", Gates: []string{"implement.test.tests"}}),
			bad:     Event{Type: EventGateShortCircuited, NodeID: "implement.test.tests", Actor: "human:johan", EvidenceHash: workHash1},
			wantErr: "requires an engine actor",
		},
		{
			name:    "short-circuit requires a prior verdict",
			events:  base,
			bad:     Event{Type: EventGateShortCircuited, NodeID: "implement.test.tests", Actor: ActorEvidenceUnchanged, EvidenceHash: workHash1},
			wantErr: "no prior verdict",
		},
		{
			name:    "short-circuit rejects settled gates",
			events:  append(append([]Event(nil), failedGate...), Event{Type: EventFeedbackRecorded, Seq: seq + 2, NodeID: "implement.do", FromNodeID: "implement.test.tests", Feedback: "x"}),
			bad:     Event{Type: EventGateShortCircuited, NodeID: "implement.test.tests", Actor: ActorEvidenceUnchanged, EvidenceHash: workHash1},
			wantErr: "only a re-entering gate can short-circuit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := applyAll(t, tt.events)
			bad := tt.bad
			bad.Seq = st.LastLogSeq + 1
			if _, err := Apply(st, bad); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestGateLoopInvariants(t *testing.T) {
	stageChild := func(stage model.StageKind, mutate func(*NodeState)) State {
		nodes := map[string]NodeState{
			"implement": {Type: model.NodeTypeTask, Status: NodeStatusRunning, Children: []string{"implement.plan", "implement.do", "implement.test.tests", "implement.done"}},
			"implement.plan": {Parent: "implement", Stage: model.StagePlan, Status: NodeStatusCompleted,
				Attempt: 1, ActiveAttempt: &AttemptState{Attempt: 1, Actor: "agent:agt_dev", Outcome: "pass", EvidenceRef: "e1", StartedAt: testTime, SettledAt: testTime}},
			"implement.do": {Parent: "implement", Stage: model.StageDo, Status: NodeStatusCompleted,
				Attempt: 1, ActiveAttempt: &AttemptState{Attempt: 1, Actor: "agent:agt_dev", Outcome: "pass", EvidenceRef: "e2", StartedAt: testTime, SettledAt: testTime}},
			"implement.test.tests": {Parent: "implement", Stage: model.StageTest, StepID: "tests", Status: NodeStatusReady},
			"implement.done":       {Parent: "implement", Stage: model.StageDone, Status: NodeStatusPending},
		}
		node := nodes["implement."+string(stage)]
		key := "implement." + string(stage)
		if stage == model.StageTest {
			key = "implement.test.tests"
			node = nodes[key]
		}
		mutate(&node)
		nodes[key] = node
		st := stateWithNodes(nodes)
		st.RunID = "run_1"
		return st
	}

	t.Run("pending feedback on a gate is flagged", func(t *testing.T) {
		st := stageChild(model.StageTest, func(n *NodeState) {
			n.PendingFeedback = &FeedbackRef{FromNodeID: "implement.test.tests"}
		})
		if !hasDiagnostic(CheckInvariants(&st), "pending_feedback_on_non_work_stage") {
			t.Fatalf("diags = %#v", CheckInvariants(&st))
		}
	})

	t.Run("pending feedback from a non-gate sibling is flagged", func(t *testing.T) {
		st := stageChild(model.StageDo, func(n *NodeState) {
			n.PendingFeedback = &FeedbackRef{FromNodeID: "implement.plan"}
		})
		if !hasDiagnostic(CheckInvariants(&st), "pending_feedback_bad_source") {
			t.Fatalf("diags = %#v", CheckInvariants(&st))
		}
	})

	t.Run("engine decision on a non-gate node is flagged", func(t *testing.T) {
		st := stageChild(model.StageDo, func(n *NodeState) {
			n.Decisions = []DecisionRecord{{Actor: ActorEvidenceUnchanged, Verdict: "pass"}}
		})
		if !hasDiagnostic(CheckInvariants(&st), "engine_decision_on_non_gate") {
			t.Fatalf("diags = %#v", CheckInvariants(&st))
		}
	})

	t.Run("engine decision without predecessor is flagged", func(t *testing.T) {
		st := stageChild(model.StageTest, func(n *NodeState) {
			n.Decisions = []DecisionRecord{{Actor: ActorEvidenceUnchanged, Verdict: "pass"}}
		})
		if !hasDiagnostic(CheckInvariants(&st), "engine_decision_without_prior_verdict") {
			t.Fatalf("diags = %#v", CheckInvariants(&st))
		}
	})

	t.Run("engine decision diverging from prior is flagged", func(t *testing.T) {
		st := stageChild(model.StageTest, func(n *NodeState) {
			n.Decisions = []DecisionRecord{
				{Actor: "program:go test@exit1", Verdict: "fail", EvidenceRef: "e3"},
				{Actor: ActorEvidenceUnchanged, Verdict: "pass", EvidenceRef: "e3"},
			}
		})
		if !hasDiagnostic(CheckInvariants(&st), "engine_decision_diverges_from_prior") {
			t.Fatalf("diags = %#v", CheckInvariants(&st))
		}
	})

	t.Run("tampered fail count over template budget is flagged", func(t *testing.T) {
		st := stageChild(model.StageTest, func(n *NodeState) {
			n.FailCount = 4
		})
		tmpl := &model.Template{
			ID:    "demo",
			Start: "implement",
			Nodes: map[string]model.Node{
				"implement": {
					Type:      model.NodeTypeTask,
					Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "do it"},
					Plan:      &model.Step{ID: "plan", Performer: model.Performer{Kind: model.PerformerAgent, Prompt: "plan"}},
					Checks:    []model.Step{{ID: "tests", Retry: &model.RetryPolicy{MaxAttempts: 3}, Performer: model.Performer{Kind: model.PerformerProgram, Run: "go test"}}},
					Next:      model.Next{"pass": "end"},
				},
				"end": {Type: model.NodeTypeEnd},
			},
		}
		diags := CheckTemplateInvariants(&st, tmpl)
		if !hasDiagnostic(diags, "gate_fail_count_over_budget") {
			t.Fatalf("diags = %#v", diags)
		}
		// Within budget passes.
		ok := stageChild(model.StageTest, func(n *NodeState) { n.FailCount = 3 })
		if hasDiagnostic(CheckTemplateInvariants(&ok, tmpl), "gate_fail_count_over_budget") {
			t.Fatalf("in-budget count must pass: %#v", CheckTemplateInvariants(&ok, tmpl))
		}
	})
}
