package worklist

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestDeriveProjectsObligationKindsAndNudgeSchedule(t *testing.T) {
	created := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	next := created.Add(30 * time.Minute)
	last := created.Add(5 * time.Minute)
	st := &state.State{
		RunID: "run-one",
		Nodes: map[string]state.NodeState{
			"approve": {Type: model.NodeTypeDecision, Status: state.NodeStatusWaitingHuman},
			"review":  {Type: model.NodeTypeTask, Stage: model.StageReview, Status: state.NodeStatusWaitingHuman},
			"wait":    {Type: model.NodeTypeTask, Status: state.NodeStatusWaitingHuman},
			"agent":   {Type: model.NodeTypeTask, Status: state.NodeStatusWaitingAgent},
		},
		Obligations: map[string]state.ObligationRecord{
			"decision-slot": obligation("decision-slot", "run-one", "approve", "decision-cmd", state.WaitKindHuman, "human:johan", created),
			"review-slot":   obligation("review-slot", "run-one", "review", "review-cmd", state.WaitKindHuman, "human:johan", created),
			"wait-slot":     obligation("wait-slot", "run-one", "wait", "wait-cmd", state.WaitKindHuman, "role:operator", created),
			"agent-slot":    obligation("agent-slot", "run-one", "agent", "agent-cmd", state.WaitKindAgent, "agent:agt_worker", created),
		},
		Contacts: map[string]state.ContactState{
			"decision-cmd": {
				CommandID: "decision-cmd", Kind: state.WaitKindHuman, Assignee: "human:johan",
				Budget: 5, Used: 2, EscalationTarget: "human:operator",
				LastContactedAt: last, NextContactAt: next, Paused: true,
			},
		},
	}
	items := Derive([]store.Snapshot{{Run: store.RunRecord{ID: "run-one"}, State: st}})
	if len(items) != 4 {
		t.Fatalf("items = %#v", items)
	}
	byNode := itemNodes(items)
	if byNode["approve"].Kind != KindDecisionNeeded || byNode["review"].Kind != KindReviewNeeded ||
		byNode["wait"].Kind != KindHumanWait || byNode["agent"].Kind != KindAgentObligation {
		t.Fatalf("kinds = %#v", byNode)
	}
	nudge := byNode["approve"].Nudge
	if nudge == nil || nudge.BudgetUsed != 2 || nudge.BudgetMax != 5 || !nudge.LastContactAt.Equal(last) ||
		!nudge.NextContactAt.Equal(next) || nudge.EscalationTarget != "human:operator" || !nudge.Paused {
		t.Fatalf("nudge = %#v", nudge)
	}
}

func TestEveryVerifiedHumanWaitDerivesOneItem(t *testing.T) {
	st := &state.State{
		RunID: "verified-waits",
		Nodes: map[string]state.NodeState{
			"one": {Status: state.NodeStatusWaitingHuman},
			"two": {Status: state.NodeStatusWaitingHuman, Stage: model.StagePlanApproval},
		},
		Obligations: map[string]state.ObligationRecord{
			"one-slot": obligation("one-slot", "verified-waits", "one", "one-cmd", state.WaitKindHuman, "human:one", time.Now()),
			"two-slot": obligation("two-slot", "verified-waits", "two", "two-cmd", state.WaitKindHuman, "human:two", time.Now()),
		},
	}
	if diagnostics := state.WaitingNodesHaveWaitRecords(st); diagnostics.HasErrors() {
		t.Fatalf("verify fixture invalid: %#v", diagnostics)
	}
	items := Derive([]store.Snapshot{{Run: store.RunRecord{ID: st.RunID}, State: st}})
	byNode := itemNodes(items)
	for nodeID, node := range st.Nodes {
		if node.Status != state.NodeStatusWaitingHuman {
			continue
		}
		if _, ok := byNode[nodeID]; !ok {
			t.Fatalf("verified human wait %q has no work item: %#v", nodeID, items)
		}
	}
}

func TestBlockedMirrorDerivesCanonicalItemAndResolvedReplay(t *testing.T) {
	resolution := &state.BlockResolution{
		NodeID: "implement.test.tests", BlockedAttempt: 2, Decision: state.BlockDecisionRetry,
		Actor: "human:operator", Reason: "transient failure reviewed", EvidenceRef: "worklist:one",
	}
	active := store.Snapshot{Run: store.RunRecord{ID: "blocked-run"}, State: &state.State{Nodes: map[string]state.NodeState{
		"implement": {
			Status: state.NodeStatusBlocked, Children: []string{"implement.test.tests"},
			BlockedAttempt: 2, BlockedNodeID: "implement.test.tests", BlockedReason: "tests exhausted", BlockedOwner: "human:oncall",
		},
		"implement.test.tests": {
			Status: state.NodeStatusBlocked, Parent: "implement", Attempt: 2,
			BlockedAttempt: 2, BlockedNodeID: "implement.test.tests", BlockedReason: "tests exhausted", BlockedOwner: "human:oncall",
			ActiveAttempt: &state.AttemptState{Attempt: 2, EvidenceRef: "artifact:test-output"},
		},
	}}}
	items := Derive([]store.Snapshot{active})
	if len(items) != 1 || items[0].Node != "implement.test.tests" || items[0].Kind != KindBlocked ||
		items[0].Assignee != "human:oncall" || items[0].Summary != "tests exhausted" || items[0].Nudge != nil {
		t.Fatalf("blocked items = %#v", items)
	}
	if len(items[0].Links.EvidenceRefs) != 1 || items[0].Links.EvidenceRefs[0] != "artifact:test-output" {
		t.Fatalf("blocked evidence links = %#v", items[0].Links.EvidenceRefs)
	}
	wantID := items[0].ID
	child := active.State.Nodes["implement.test.tests"]
	child.Status = state.NodeStatusReady
	child.BlockedReason = ""
	child.BlockedOwner = ""
	child.BlockResolution = resolution
	active.State.Nodes["implement.test.tests"] = child
	parent := active.State.Nodes["implement"]
	parent.Status = state.NodeStatusRunning
	parent.BlockedReason = ""
	parent.BlockedOwner = ""
	parent.BlockResolution = resolution
	active.State.Nodes["implement"] = parent
	resolved := Derive([]store.Snapshot{active})
	if len(resolved) != 1 || resolved[0].ID != wantID || resolved[0].Status != state.WaitStatusSatisfied ||
		resolved[0].Summary != resolution.Reason || resolved[0].Assignee != string(resolution.Actor) {
		t.Fatalf("resolved item = %#v", resolved)
	}
}

func TestEmptyDerivationIsAnEmptyList(t *testing.T) {
	items := Derive(nil)
	if items == nil || len(items) != 0 {
		t.Fatalf("empty worklist = %#v", items)
	}
}

func TestFilterAndStableIDs(t *testing.T) {
	created := time.Now()
	st := &state.State{RunID: "run", Nodes: map[string]state.NodeState{
		"a": {Status: state.NodeStatusWaitingHuman}, "b": {Status: state.NodeStatusWaitingHuman},
	}, Obligations: map[string]state.ObligationRecord{
		"a-slot": obligation("a-slot", "run", "a", "a-cmd", state.WaitKindHuman, "human:a", created),
		"b-slot": obligation("b-slot", "run", "b", "b-cmd", state.WaitKindHuman, "human:b", created),
	}}
	snapshot := store.Snapshot{Run: store.RunRecord{ID: "run"}, State: st}
	first, second := Derive([]store.Snapshot{snapshot}), Derive([]store.Snapshot{snapshot})
	if len(first) != 2 || first[0].ID != second[0].ID || first[1].ID != second[1].ID {
		t.Fatalf("ids are not stable: %#v / %#v", first, second)
	}
	filtered := ApplyFilter(first, Filter{Assignee: "human:b", Kind: KindHumanWait, Run: "run", Status: state.WaitStatusPending})
	if len(filtered) != 1 || filtered[0].Node != "b" {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func obligation(id, runID, nodeID, commandID string, kind state.WaitKind, assignee string, created time.Time) state.ObligationRecord {
	return state.ObligationRecord{
		ID: id, RunID: runID, NodeID: nodeID, Attempt: 1, CommandID: commandID,
		Kind: kind, Assignee: assignee, Status: state.WaitStatusPending,
		CreatedAt: created, DueAt: created.Add(time.Hour), Summary: "Complete " + nodeID,
		AvailableActions: []string{"approve", "reject", "ask-changes"},
	}
}

func itemNodes(items []Item) map[string]Item {
	out := make(map[string]Item, len(items))
	for _, item := range items {
		out[item.Node] = item
	}
	return out
}
