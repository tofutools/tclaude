package worklist

import (
	"context"
	"errors"
	"testing"
	"time"

	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
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

func TestEveryHumanWaitInVerifiedStoreSnapshotDerivesOneItem(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "verified-worklist", Start: "approve",
		Nodes: map[string]model.Node{
			"approve": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerHuman, Profile: "johan", Ask: "Approve release?"},
				Next:      model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	initial := state.New("verified-worklist", record.Ref, record.Ref, []state.NodeInit{
		{ID: "approve", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "verified-worklist", TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), "verified-worklist")
	if err != nil {
		t.Fatal(err)
	}
	commands, err := plan.Plan(snapshot.State, tmpl)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands=%#v err=%v", commands, err)
	}
	executor := processexec.New(fs, map[model.PerformerKind]processexec.Adapter{
		model.PerformerHuman: verifiedHumanAdapter{},
	})
	if _, err := executor.Execute(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	snapshot, err = fs.LoadRun(t.Context(), "verified-worklist")
	if err != nil {
		t.Fatal(err)
	}
	if report := processverify.SnapshotWithTemplate(snapshot, tmpl); report.HasErrors() {
		t.Fatalf("fixture must pass complete verification: %#v", report.Diagnostics)
	}
	items := Derive([]store.Snapshot{snapshot})
	byNode := itemNodes(items)
	for nodeID, node := range snapshot.State.Nodes {
		if node.Status != state.NodeStatusWaitingHuman {
			continue
		}
		if _, ok := byNode[nodeID]; !ok {
			t.Fatalf("verified human wait %q has no work item: %#v", nodeID, items)
		}
	}
}

func TestBlockedMirrorDerivesCanonicalItemAndResolvedReplay(t *testing.T) {
	blockedAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	resolution := &state.BlockResolution{
		NodeID: "implement.test.tests", BlockedAttempt: 2, Decision: state.BlockDecisionRetry,
		Actor: "human:operator", Reason: "transient failure reviewed", EvidenceRef: "worklist:one",
		Timestamp: blockedAt.Add(time.Hour),
	}
	active := store.Snapshot{Run: store.RunRecord{ID: "blocked-run"}, State: &state.State{StateSchemaVersion: state.StateSchemaVersion, Nodes: map[string]state.NodeState{
		"implement": {
			Status: state.NodeStatusBlocked, Children: []string{"implement.test.tests"},
			BlockedAttempt: 2, BlockedNodeID: "implement.test.tests", BlockedReason: "tests exhausted", BlockedOwner: "human:oncall",
			BlockedAt: blockedAt,
		},
		"implement.test.tests": {
			Status: state.NodeStatusBlocked, Parent: "implement", Attempt: 2,
			BlockedAttempt: 2, BlockedNodeID: "implement.test.tests", BlockedReason: "tests exhausted", BlockedOwner: "human:oncall",
			BlockedAt:     blockedAt,
			ActiveAttempt: &state.AttemptState{Attempt: 2, EvidenceRef: "artifact:test-output"},
		},
	}, OutstandingCommands: map[string]state.OutstandingCommand{
		"block-cmd": {ID: "block-cmd", NodeID: "implement.test.tests", Attempt: 2, Kind: state.CommandKindBlockNode, Status: state.CommandStatusObserved},
	}, Contacts: map[string]state.ContactState{
		"block-cmd": {CommandID: "block-cmd", Kind: state.WaitKindHuman, Assignee: "human:oncall", Cadence: "30m0s", Budget: 5, Used: 1, EscalationTarget: "human:operator", NextContactAt: blockedAt.Add(30 * time.Minute)},
	}}}
	items := Derive([]store.Snapshot{active})
	if len(items) != 1 || items[0].Node != "implement.test.tests" || items[0].Kind != KindBlocked ||
		items[0].Assignee != "human:oncall" || items[0].Summary != "tests exhausted" || !items[0].CreatedAt.Equal(blockedAt) ||
		!items[0].ChangedAt.Equal(blockedAt) || items[0].Nudge == nil || items[0].Nudge.BudgetUsed != 1 || items[0].Nudge.BudgetMax != 5 {
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
	contact := active.State.Contacts["block-cmd"]
	contact.Paused = true
	contact.PauseReason = "block resolved"
	contact.NextContactAt = time.Time{}
	active.State.Contacts["block-cmd"] = contact
	resolved := Derive([]store.Snapshot{active})
	if len(resolved) != 1 || resolved[0].ID != wantID || resolved[0].Status != state.WaitStatusSatisfied ||
		resolved[0].Summary != resolution.Reason || resolved[0].Assignee != string(resolution.Actor) ||
		!resolved[0].CreatedAt.Equal(blockedAt) || !resolved[0].ChangedAt.Equal(resolution.Timestamp) || resolved[0].Nudge == nil || !resolved[0].Nudge.Paused {
		t.Fatalf("resolved item = %#v", resolved)
	}
}

func TestLegacyBlockedItemDoesNotFabricateTimelineOrNudge(t *testing.T) {
	legacy := &state.State{StateSchemaVersion: 5, Nodes: map[string]state.NodeState{
		"blocked": {Status: state.NodeStatusBlocked, Attempt: 1, BlockedAttempt: 1, BlockedNodeID: "blocked", BlockedReason: "legacy", BlockedOwner: "human:operator"},
	}}
	items := Derive([]store.Snapshot{{Run: store.RunRecord{ID: "legacy"}, State: legacy}})
	if len(items) != 1 || !items[0].CreatedAt.IsZero() || !items[0].ChangedAt.IsZero() || items[0].Nudge != nil {
		t.Fatalf("legacy blocked item = %#v", items)
	}
}

func TestEmptyDerivationIsAnEmptyList(t *testing.T) {
	items := Derive(nil)
	if items == nil || len(items) != 0 {
		t.Fatalf("empty worklist = %#v", items)
	}
}

func TestDerivePathV1KeepsLiveDetachedWaitAndNeverSynthesizesJoinWork(t *testing.T) {
	fs, runID := pathV1DetachedWaitRun(t)
	executor := processexec.NewExclusiveV7(fs, map[model.PerformerKind]processexec.Adapter{
		model.PerformerAgent: pathV1PassAdapter{},
	})
	var checkpoint *pathv1.CheckpointV7
	var err error
	for range 4 {
		checkpoint, err = executor.Drive(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if pathv1.CurrentRunStatus(checkpoint) == "completed" {
			break
		}
	}
	if status := pathv1.CurrentRunStatus(checkpoint); status != "running" && status != "completed" {
		t.Fatalf("run status = %q", status)
	}
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := DerivePathV1(t.Context(), snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wait *Item
	for index := range items {
		item := &items[index]
		if item.Node == "merge" {
			t.Fatalf("synthetic join work item: %#v", item)
		}
		if item.Node == "wait" {
			wait = item
		}
	}
	if wait == nil || wait.Kind != KindWaiting || wait.Status != state.WaitStatusPending ||
		!wait.Detached || wait.DetachmentCount < 1 || wait.Target.CommandID != "" || len(wait.AvailableActions) != 0 {
		t.Fatalf("detached wait item = %#v; all items = %#v", wait, items)
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

type verifiedHumanAdapter struct{}

func (verifiedHumanAdapter) Validate(processexec.Request) error { return nil }

func (verifiedHumanAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, errors.New("human performer is deferred")
}

func (verifiedHumanAdapter) Dispatch(_ context.Context, request processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{
		ExternalRef: "obligation:" + request.Command.ID, Assignee: "human:johan",
		Summary: "Approve release?", AvailableActions: []string{"approve", "reject"}, CreateObligation: true,
	}, nil
}

func (verifiedHumanAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}

type pathV1PassAdapter struct{}

func (pathV1PassAdapter) Validate(processexec.Request) error { return nil }

func (pathV1PassAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{Actor: "agent:agt_test1", Verdict: "pass"}, nil
}

func pathV1DetachedWaitRun(t *testing.T) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "worklist-detached-wait", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"wait": "wait", "work": "work"}},
			"wait":  {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Signal: "release"}, Next: model.Next{"pass": "merge"}},
			"work":  {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "finish first"}, Next: model.Next{"pass": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Join: model.JoinAny, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-worklist-detached-wait"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "fork", Type: model.NodeTypeParallel, Status: state.NodeStatusReady},
		{ID: "wait", Type: model.NodeTypeWait, Status: state.NodeStatusPending},
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "merge", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.InitializePathV1(t.Context(), runID, proof); err != nil {
		t.Fatal(err)
	}
	return fs, runID
}
