package state

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func compoundChildInits() []NodeInit {
	return []NodeInit{
		{ID: "implement.plan", Parent: "implement", Stage: model.StagePlan},
		{ID: "implement.do", Parent: "implement", Stage: model.StageDo},
		{ID: "implement.test.tests", Parent: "implement", Stage: model.StageTest, StepID: "tests"},
		{ID: "implement.review", Parent: "implement", Stage: model.StageReview},
		{ID: "implement.done", Parent: "implement", Stage: model.StageDone},
	}
}

func expandedState(t *testing.T) State {
	t.Helper()
	st, err := ApplyAll(State{}, []Event{
		{Type: EventRunInitialized, Seq: 1, RunID: "run_1", OriginalTemplateRef: "demo@sha256:x", CurrentTemplateRef: "demo@sha256:x", Nodes: []NodeInit{
			{ID: "implement", Type: model.NodeTypeTask, Status: NodeStatusReady},
			{ID: "end", Type: model.NodeTypeEnd},
		}},
		{Type: EventNodeExpanded, Seq: 2, NodeID: "implement", Nodes: compoundChildInits()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestNodeExpandedRecordsChildren(t *testing.T) {
	st := expandedState(t)
	parent := st.Nodes["implement"]
	if parent.Status != NodeStatusRunning {
		t.Fatalf("parent status = %s, want running", parent.Status)
	}
	want := []string{"implement.plan", "implement.do", "implement.test.tests", "implement.review", "implement.done"}
	if strings.Join(parent.Children, " ") != strings.Join(want, " ") {
		t.Fatalf("children = %v, want %v", parent.Children, want)
	}
	if st.Nodes["implement.plan"].Status != NodeStatusReady {
		t.Fatalf("first child must be ready, got %s", st.Nodes["implement.plan"].Status)
	}
	for _, childID := range want[1:] {
		child := st.Nodes[childID]
		if child.Status != NodeStatusPending {
			t.Fatalf("child %s status = %s, want pending", childID, child.Status)
		}
		if child.Parent != "implement" {
			t.Fatalf("child %s parent = %q", childID, child.Parent)
		}
	}
	if st.Nodes["implement.test.tests"].StepID != "tests" {
		t.Fatalf("test child must carry step id")
	}
	if diags := CheckInvariants(&st); diags.HasErrors() {
		t.Fatalf("expanded state must satisfy invariants: %#v", diags.Errors())
	}
}

func TestNodeExpandedRejections(t *testing.T) {
	base := func(t *testing.T) State {
		t.Helper()
		st, err := Apply(State{}, Event{Type: EventRunInitialized, Seq: 1, RunID: "run_1", Nodes: []NodeInit{
			{ID: "implement", Type: model.NodeTypeTask, Status: NodeStatusReady},
			{ID: "implement.do", Type: model.NodeTypeTask},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	tests := []struct {
		name    string
		mutate  func(st *State)
		event   Event
		wantErr string
	}{
		{
			name:    "child id already declared",
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.do", Stage: model.StageDo}, {ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "already declared",
		},
		{
			name:    "bad prefix",
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "other.do", Stage: model.StageDo}, {ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "must be prefixed",
		},
		{
			name:    "invalid stage",
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.work", Stage: "work"}, {ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "invalid stage",
		},
		{
			name:    "test stage requires step id",
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.test.x", Stage: model.StageTest}, {ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "inconsistent",
		},
		{
			name:    "done must be last",
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.done", Stage: model.StageDone}, {ID: "implement.work2", Stage: model.StageDo}}},
			wantErr: "done stage",
		},
		{
			name:    "requires at least two children",
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "at least one work stage",
		},
		{
			name: "already expanded",
			mutate: func(st *State) {
				node := st.Nodes["implement"]
				node.Children = []string{"implement.do"}
				st.Nodes["implement"] = node
			},
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.x", Stage: model.StageDo}, {ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "already expanded",
		},
		{
			name: "not ready",
			mutate: func(st *State) {
				node := st.Nodes["implement"]
				node.Status = NodeStatusPending
				st.Nodes["implement"] = node
			},
			event:   Event{Type: EventNodeExpanded, NodeID: "implement", Nodes: []NodeInit{{ID: "implement.x", Stage: model.StageDo}, {ID: "implement.done", Stage: model.StageDone}}},
			wantErr: "only ready nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := base(t)
			if tt.mutate != nil {
				tt.mutate(&st)
			}
			if _, err := Apply(st, tt.event); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestStageChildPassWithoutEvidenceFlipsToFailed(t *testing.T) {
	st := expandedState(t)
	st, err := ApplyAll(st, []Event{
		{Type: EventNodeAttemptStarted, Seq: 3, NodeID: "implement.plan", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 4, NodeID: "implement.plan", Outcome: "pass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	node := st.Nodes["implement.plan"]
	if node.Status != NodeStatusFailed {
		t.Fatalf("claimed pass without evidence must flip to failed, got %s", node.Status)
	}

	st = expandedState(t)
	st, err = ApplyAll(st, []Event{
		{Type: EventNodeAttemptStarted, Seq: 3, NodeID: "implement.plan", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 4, NodeID: "implement.plan", Outcome: "pass", EvidenceRef: "artifacts/plan.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	node = st.Nodes["implement.plan"]
	if node.Status != NodeStatusCompleted {
		t.Fatalf("pass with evidence must complete, got %s", node.Status)
	}
	if node.ActiveAttempt == nil || node.ActiveAttempt.EvidenceRef != "artifacts/plan.md" {
		t.Fatalf("settled attempt must record the evidence ref: %#v", node.ActiveAttempt)
	}
}

func TestCompoundLinkageInvariants(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(st *State)
		code   string
	}{
		{
			name: "child missing from parent list",
			mutate: func(st *State) {
				node := st.Nodes["implement"]
				node.Children = node.Children[:len(node.Children)-1]
				st.Nodes["implement"] = node
			},
			code: "stage_child_not_in_parent",
		},
		{
			name: "blocked child with unblocked parent",
			mutate: func(st *State) {
				child := st.Nodes["implement.do"]
				child.Status = NodeStatusBlocked
				child.BlockedReason = "x"
				child.BlockedOwner = "human:operator"
				st.Nodes["implement.do"] = child
			},
			code: "blocked_child_unblocked_parent",
		},
		{
			name: "blocked parent without blocked child",
			mutate: func(st *State) {
				node := st.Nodes["implement"]
				node.Status = NodeStatusBlocked
				node.BlockedReason = "x"
				node.BlockedOwner = "human:operator"
				st.Nodes["implement"] = node
			},
			code: "blocked_parent_without_blocked_child",
		},
		{
			name: "parent completed without done",
			mutate: func(st *State) {
				node := st.Nodes["implement"]
				node.Status = NodeStatusCompleted
				st.Nodes["implement"] = node
			},
			code: "expanded_parent_completed_without_done",
		},
		{
			name: "parent running after done completed",
			mutate: func(st *State) {
				done := st.Nodes["implement.done"]
				done.Status = NodeStatusCompleted
				st.Nodes["implement.done"] = done
			},
			code: "expanded_parent_running_after_done",
		},
		{
			name: "nested expansion",
			mutate: func(st *State) {
				child := st.Nodes["implement.do"]
				child.Children = []string{"implement.do.x"}
				st.Nodes["implement.do"] = child
			},
			code: "nested_expansion",
		},
		{
			name: "stage metadata without parent",
			mutate: func(st *State) {
				node := st.Nodes["end"]
				node.Stage = model.StageDo
				st.Nodes["end"] = node
			},
			code: "stage_without_parent",
		},
		{
			name: "duplicate child listing",
			mutate: func(st *State) {
				node := st.Nodes["implement"]
				node.Children = append([]string{"implement.plan"}, node.Children...)
				st.Nodes["implement"] = node
			},
			code: "duplicate_expansion_child",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := expandedState(t)
			tt.mutate(&st)
			diags := CompoundLinkageIsConsistent(&st)
			if !hasDiagCode(diags, tt.code) {
				t.Fatalf("missing %q diagnostic: %#v", tt.code, diags)
			}
		})
	}
}

func TestResolvedBlockMirrorAndAuditInvariants(t *testing.T) {
	resolution := BlockResolution{
		NodeID: "implement.plan", BlockedAttempt: 1, Decision: BlockDecisionSkip,
		Actor: "human:johan", Reason: "plan accepted out of band", EvidenceRef: "decision:42", Timestamp: testTime,
	}
	valid := expandedState(t)
	child := valid.Nodes[resolution.NodeID]
	child.Status = NodeStatusSkipped
	child.BlockedAttempt = resolution.BlockedAttempt
	child.BlockedNodeID = resolution.NodeID
	child.BlockedAt = testTime
	child.BlockResolution = &resolution
	valid.Nodes[resolution.NodeID] = child
	parent := valid.Nodes["implement"]
	parent.BlockedAttempt = resolution.BlockedAttempt
	parent.BlockedNodeID = resolution.NodeID
	parent.BlockedAt = testTime
	parent.BlockResolution = &resolution
	valid.Nodes["implement"] = parent
	valid.AdminRecords = append(valid.AdminRecords, AdminRecord{
		Type: EventBlockResolutionRecorded, Actor: resolution.Actor, Reason: resolution.Reason,
		EvidenceRef: resolution.EvidenceRef, Timestamp: resolution.Timestamp, Resolution: &resolution,
	})
	valid.OutstandingCommands["cmd_block"] = *blockCommandForTest("cmd_block", resolution.NodeID, 1, "human:operator", CommandStatusObserved)
	valid.Contacts["cmd_block"] = ContactState{
		CommandID: "cmd_block", Kind: WaitKindHuman, Assignee: "human:operator", Cadence: "30m0s", Budget: 5,
		EscalationTarget: "human:operator", Paused: true, PauseReason: "block resolved",
	}
	if diagnostics := CheckInvariants(&valid); diagnostics.HasErrors() {
		t.Fatalf("resolved blocked state must verify: %#v", diagnostics)
	}

	tests := []struct {
		name   string
		mutate func(*State)
		code   string
	}{
		{
			name: "child cleared but parent remains blocked",
			mutate: func(st *State) {
				parent := st.Nodes["implement"]
				parent.Status = NodeStatusBlocked
				parent.BlockedReason = "poison"
				parent.BlockedOwner = "human:operator"
				parent.BlockResolution = nil
				st.Nodes["implement"] = parent
			},
			code: "blocked_parent_without_blocked_child",
		},
		{
			name: "parent cleared but child remains blocked",
			mutate: func(st *State) {
				child := st.Nodes[resolution.NodeID]
				child.Status = NodeStatusBlocked
				child.BlockedReason = "poison"
				child.BlockedOwner = "human:operator"
				child.BlockResolution = nil
				st.Nodes[resolution.NodeID] = child
			},
			code: "blocked_child_unblocked_parent",
		},
		{
			name:   "resolution without audit",
			mutate: func(st *State) { st.AdminRecords = nil },
			code:   "block_resolution_without_audit",
		},
		{
			name: "parent and child carry different audited resolutions",
			mutate: func(st *State) {
				other := resolution
				other.Decision = BlockDecisionRetry
				other.Reason = "retry instead"
				parent := st.Nodes["implement"]
				parent.BlockResolution = &other
				st.Nodes["implement"] = parent
				st.AdminRecords = append(st.AdminRecords, AdminRecord{
					Type: EventBlockResolutionRecorded, Actor: other.Actor, Reason: other.Reason,
					EvidenceRef: other.EvidenceRef, Timestamp: other.Timestamp, Resolution: &other,
				})
			},
			code: "block_mirror_resolution_mismatch",
		},
		{
			name: "parent and child carry different audited attempts",
			mutate: func(st *State) {
				other := resolution
				other.BlockedAttempt = 2
				parent := st.Nodes["implement"]
				parent.BlockedAttempt = 2
				parent.BlockResolution = &other
				st.Nodes["implement"] = parent
				st.AdminRecords = append(st.AdminRecords, AdminRecord{
					Type: EventBlockResolutionRecorded, Actor: other.Actor, Reason: other.Reason,
					EvidenceRef: other.EvidenceRef, Timestamp: other.Timestamp, Resolution: &other,
				})
			},
			code: "block_mirror_tombstone_mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := Clone(valid)
			tt.mutate(&st)
			if diagnostics := CheckInvariants(&st); !hasDiagnostic(diagnostics, tt.code) {
				t.Fatalf("expected %q, got %#v", tt.code, diagnostics)
			}
		})
	}
}

func TestParentBlockGenerationIsScopedToPoisonedChild(t *testing.T) {
	st := expandedState(t)
	resolution := BlockResolution{
		NodeID: "implement.plan", BlockedAttempt: 1, Decision: BlockDecisionSkip,
		Actor: "human:johan", Reason: "skip plan", EvidenceRef: "decision:plan", Timestamp: testTime,
	}
	parent := st.Nodes["implement"]
	parent.BlockedAttempt = 1
	parent.BlockedNodeID = resolution.NodeID
	parent.BlockResolution = &resolution
	st.Nodes["implement"] = parent

	// A delayed block for the resolved child is a no-op.
	stale, err := Apply(st, Event{
		Type: EventNodeBlocked, At: testTime, NodeID: "implement", FromNodeID: resolution.NodeID,
		Attempt: 1, Reason: "old poison", Owner: "human:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Nodes["implement"].Status != NodeStatusRunning {
		t.Fatalf("stale poison re-blocked parent: %#v", stale.Nodes["implement"])
	}

	// Another stage may poison on its own attempt 1; the old child's
	// generation tombstone must not suppress that distinct block.
	newer, err := Apply(stale, Event{
		Type: EventNodeBlocked, At: testTime, NodeID: "implement", FromNodeID: "implement.test.tests",
		Attempt: 1, Reason: "test poison", Owner: "human:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent = newer.Nodes["implement"]
	if parent.Status != NodeStatusBlocked || parent.BlockedNodeID != "implement.test.tests" || parent.BlockResolution != nil {
		t.Fatalf("new child's poison was suppressed: %#v", parent)
	}
}

func TestStageSkipForgeryIsDetected(t *testing.T) {
	// Forged completion: done stage and parent claim completion while the do
	// and test stages never ran. Verify must not bless the run.
	st := expandedState(t)
	for _, nodeID := range []string{"implement.done", "implement"} {
		node := st.Nodes[nodeID]
		node.Status = NodeStatusCompleted
		st.Nodes[nodeID] = node
	}
	diags := CompoundLinkageIsConsistent(&st)
	if !hasDiagCode(diags, "expanded_parent_completed_with_incomplete_stages") {
		t.Fatalf("forged completion with skipped stages must be flagged: %#v", diags)
	}

	// Out-of-order activation: the review stage is ready while do is pending.
	st = expandedState(t)
	review := st.Nodes["implement.review"]
	review.Status = NodeStatusReady
	st.Nodes["implement.review"] = review
	diags = CompoundLinkageIsConsistent(&st)
	if !hasDiagCode(diags, "stage_activated_out_of_order") {
		t.Fatalf("out-of-order stage activation must be flagged: %#v", diags)
	}
}

func TestNodeStatusSetGuardsStageChain(t *testing.T) {
	st := expandedState(t)
	// implement.plan is ready but not completed; skipping ahead to ready or
	// complete a later stage must be rejected by the reducer.
	if _, err := Apply(st, Event{Type: EventNodeStatusSet, NodeID: "implement.do", NodeStatus: NodeStatusReady}); err == nil || !strings.Contains(err.Error(), "cannot activate before earlier stage") {
		t.Fatalf("err = %v, want prior-stage guard", err)
	}
	if _, err := Apply(st, Event{Type: EventNodeStatusSet, NodeID: "implement.done", NodeStatus: NodeStatusCompleted}); err == nil || !strings.Contains(err.Error(), "cannot activate before earlier stage") {
		t.Fatalf("err = %v, want prior-stage guard", err)
	}

	// The done marker settles automatically; attempts on it are rejected.
	if _, err := Apply(st, Event{Type: EventNodeAttemptStarted, NodeID: "implement.done", Actor: "human:johan", Attempt: 1}); err == nil || !strings.Contains(err.Error(), "settles automatically") {
		t.Fatalf("err = %v, want done-stage attempt rejection", err)
	}

	// The legit chain still works: settle plan with evidence, then ready do.
	st, err := ApplyAll(st, []Event{
		{Type: EventNodeAttemptStarted, Seq: 3, NodeID: "implement.plan", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 4, NodeID: "implement.plan", Outcome: "pass", EvidenceRef: "artifacts/plan.md"},
		{Type: EventNodeStatusSet, Seq: 5, NodeID: "implement.do", NodeStatus: NodeStatusReady},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes["implement.do"].Status != NodeStatusReady {
		t.Fatalf("do stage should be ready, got %s", st.Nodes["implement.do"].Status)
	}
}

func TestDoneStageCompletionCompletesParentAtomically(t *testing.T) {
	st := expandedState(t)
	events := []Event{
		{Type: EventNodeAttemptStarted, Seq: 3, NodeID: "implement.plan", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 4, NodeID: "implement.plan", Outcome: "pass", EvidenceRef: "e1"},
		{Type: EventNodeStatusSet, Seq: 5, NodeID: "implement.do", NodeStatus: NodeStatusReady},
		{Type: EventNodeAttemptStarted, Seq: 6, NodeID: "implement.do", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 7, NodeID: "implement.do", Outcome: "pass", EvidenceRef: "e2"},
		{Type: EventNodeStatusSet, Seq: 8, NodeID: "implement.test.tests", NodeStatus: NodeStatusReady},
		{Type: EventNodeAttemptStarted, Seq: 9, NodeID: "implement.test.tests", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 10, NodeID: "implement.test.tests", Outcome: "pass", EvidenceRef: "e3"},
		{Type: EventNodeStatusSet, Seq: 11, NodeID: "implement.review", NodeStatus: NodeStatusReady},
		{Type: EventNodeAttemptStarted, Seq: 12, NodeID: "implement.review", Actor: "human:johan", Attempt: 1},
		{Type: EventNodeAttemptSettled, Seq: 13, NodeID: "implement.review", Outcome: "pass", EvidenceRef: "e4"},
		{Type: EventNodeStatusSet, Seq: 14, NodeID: "implement.done", NodeStatus: NodeStatusCompleted},
	}
	st, err := ApplyAll(st, events)
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes["implement.done"].Status != NodeStatusCompleted {
		t.Fatalf("done = %#v", st.Nodes["implement.done"])
	}
	if st.Nodes["implement"].Status != NodeStatusCompleted {
		t.Fatalf("done completion must complete the parent atomically: %#v", st.Nodes["implement"])
	}
	if diags := CheckInvariants(&st); diags.HasErrors() {
		t.Fatalf("completed compound must satisfy invariants: %#v", diags.Errors())
	}
}

func TestCompletedStageChildrenHaveEvidence(t *testing.T) {
	st := expandedState(t)
	child := st.Nodes["implement.do"]
	child.Status = NodeStatusCompleted
	child.ActiveAttempt = &AttemptState{Attempt: 1, Outcome: "pass"}
	st.Nodes["implement.do"] = child
	if !hasDiagCode(CompletedStageChildrenHaveEvidence(&st), "completed_stage_child_without_evidence") {
		t.Fatal("completed stage child without evidence must be flagged")
	}
	child.ActiveAttempt.EvidenceRef = "artifacts/diff.patch"
	st.Nodes["implement.do"] = child
	if hasDiagCode(CompletedStageChildrenHaveEvidence(&st), "completed_stage_child_without_evidence") {
		t.Fatal("evidence-backed completion must not be flagged")
	}
}

func TestCheckTemplateInvariantsForExpansion(t *testing.T) {
	tmpl := &model.Template{
		ID:    "demo",
		Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "do it"},
				Plan:      &model.Step{ID: "plan", Performer: model.Performer{Kind: model.PerformerAgent, Prompt: "plan"}},
				Checks:    []model.Step{{ID: "tests", Performer: model.Performer{Kind: model.PerformerProgram, Run: "go test ./..."}}},
				Review:    &model.Step{ID: "review", Performer: model.Performer{Kind: model.PerformerAgent, Prompt: "review"}},
				Next:      model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	st := expandedState(t)
	if diags := CheckTemplateInvariants(&st, tmpl); diags.HasErrors() {
		t.Fatalf("matching expansion must verify: %#v", diags.Errors())
	}

	tampered := Clone(st)
	node := tampered.Nodes["implement"]
	node.Children = append([]string(nil), node.Children...)
	node.Children[1] = "implement.work"
	tampered.Nodes["implement"] = node
	if !hasDiagCode(CheckTemplateInvariants(&tampered, tmpl), "expansion_template_mismatch") {
		t.Fatal("tampered children list must be flagged")
	}

	unexpanded := Clone(st)
	for _, childID := range unexpanded.Nodes["implement"].Children {
		delete(unexpanded.Nodes, childID)
	}
	node = unexpanded.Nodes["implement"]
	node.Children = nil
	node.Status = NodeStatusCompleted
	unexpanded.Nodes["implement"] = node
	if !hasDiagCode(CheckTemplateInvariants(&unexpanded, tmpl), "compound_node_without_expansion") {
		t.Fatal("compound node progressing without expansion must be flagged")
	}

	plainTemplate := &model.Template{
		ID:    "demo",
		Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "do it"}, Next: model.Next{"pass": "end"}},
			"end":       {Type: model.NodeTypeEnd},
		},
	}
	if !hasDiagCode(CheckTemplateInvariants(&st, plainTemplate), "expansion_without_compound_template") {
		t.Fatal("expansion recorded against non-compound template must be flagged")
	}
}

func hasDiagCode(diags model.Diagnostics, code string) bool {
	for _, diag := range diags {
		if diag.Code == code {
			return true
		}
	}
	return false
}

func TestPoisonEscalationDecisionCannotBypassResolutionFunnel(t *testing.T) {
	tmpl := &model.Template{Nodes: map[string]model.Node{
		"implement": {
			Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"},
			Checks: []model.Step{{ID: "tests", Performer: model.Performer{Kind: model.PerformerProgram, Run: "true"}}},
			Next:   model.Next{"fail": "escalate"},
		},
		"escalate": {
			Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "retry?"},
			Next: model.Next{"retry": "implement", "cancel": "canceled"},
		},
		"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
	}}
	st := State{Nodes: map[string]NodeState{
		"implement": {
			Type: model.NodeTypeTask, Status: NodeStatusBlocked, BlockedAttempt: 2, BlockedNodeID: "implement.test.tests",
		},
		"escalate": {
			Type: model.NodeTypeDecision, Status: NodeStatusCompleted, Attempt: 2, PoisonedNodeID: "implement.test.tests",
		},
	}}
	if diags := PoisonEscalationDecisionsUseResolutionFunnel(&st, tmpl); !hasDiagCode(diags, "escalation_decision_bypassed_resolution") {
		t.Fatalf("manual escalation decision bypass verified cleanly: %#v", diags)
	}
	st.OutstandingCommands = map[string]OutstandingCommand{
		"cmd_decision": {ID: "cmd_decision", NodeID: "escalate", Kind: CommandKindRecordDecision, Status: CommandStatusObserved},
	}
	if diags := PoisonEscalationDecisionsUseResolutionFunnel(&st, tmpl); diags.HasErrors() {
		t.Fatalf("normal observed engine decision was rejected: %#v", diags)
	}
}
