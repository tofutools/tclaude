package processexec

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

func TestResolveBlockedRetryStartsFreshAttemptAndCompletes(t *testing.T) {
	marker := t.TempDir() + "/pass"
	fs, snapshot := compoundExecutorFixture(t, "test -f "+marker)
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	blocked, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempt := blocked.State.Nodes["work.test.tests"].Attempt
	request := blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)
	resolved, err := executor.ResolveBlocked(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedPair(t, resolved, state.NodeStatusReady, state.RunStatusRunning, state.BlockDecisionRetry)
	firstSeq := resolved.LastLogSeq
	replayed, err := executor.ResolveBlocked(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.LastLogSeq != firstSeq {
		t.Fatalf("idempotent retry appended events: first seq %d, replay seq %d", firstSeq, replayed.LastLogSeq)
	}
	assertResolvedPair(t, replayed, state.NodeStatusReady, state.RunStatusRunning, state.BlockDecisionRetry)
	if err := os.WriteFile(marker, []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted || finished.State.Nodes["work.test.tests"].Attempt != beforeAttempt+1 {
		t.Fatalf("retry did not complete on a fresh attempt: %#v", finished.State.Nodes["work.test.tests"])
	}
	assertRunVerifies(t, fs, snapshot.Run.ID)
}

func TestResolveBlockedSkipCompletesByDecision(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	if _, err := executor.Drive(t.Context(), snapshot.Run.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := executor.ResolveBlocked(t.Context(), blockRequest(snapshot.Run.ID, state.BlockDecisionSkip))
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedPair(t, resolved, state.NodeStatusSkipped, state.RunStatusRunning, state.BlockDecisionSkip)
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted || finished.State.Nodes["work.test.tests"].Status != state.NodeStatusSkipped {
		t.Fatalf("skip did not complete run by decision: status=%s gate=%#v", finished.State.Status, finished.State.Nodes["work.test.tests"])
	}
	assertRunVerifies(t, fs, snapshot.Run.ID)
}

func TestResolveBlockedCancelCancelsRun(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	if _, err := executor.Drive(t.Context(), snapshot.Run.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := executor.ResolveBlocked(t.Context(), blockRequest(snapshot.Run.ID, state.BlockDecisionCancel))
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedPair(t, resolved, state.NodeStatusSkipped, state.RunStatusCanceled, state.BlockDecisionCancel)
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCanceled {
		t.Fatalf("canceled run resumed: %s", finished.State.Status)
	}
	assertRunVerifies(t, fs, snapshot.Run.ID)
}

func TestStaleBlockResumeAfterRetryResolutionIsNoOp(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	block, beforeClaim := driveUntilBlockCommand(t, executor, fs, snapshot.Run.ID)
	claimed, claimedState, err := executor.claim(t.Context(), beforeClaim, block)
	if err != nil || !claimed {
		t.Fatalf("claim stale block = %v, err = %v", claimed, err)
	}
	// Simulate the paired block events landing through another worker while
	// this issued command crashes before command_observed is appended.
	contact, err := BlockedContactState(block, executorTestTime)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := fs.Append(t.Context(), snapshot.Run.ID, claimedState.LastLogSeq, []evidence.LogEntry{
		nodeEntry(block.NodeID, state.Event{Type: state.EventContactScheduled, Contact: &contact}, "", executorTestTime),
		nodeEntry(block.NodeID, state.Event{Type: state.EventNodeBlocked, Attempt: block.Attempt, Reason: block.Reason, Owner: block.Owner}, "", executorTestTime),
		nodeEntry(block.TargetNodeID, state.Event{Type: state.EventNodeBlocked, Attempt: block.Attempt, FromNodeID: block.NodeID, Reason: block.Reason, Owner: block.Owner}, "", executorTestTime),
	})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State.Nodes[block.NodeID].Status != state.NodeStatusBlocked {
		t.Fatalf("manual poison did not block: %#v", blocked.State.Nodes[block.NodeID])
	}
	request := blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)
	resolved, err := executor.ResolveBlocked(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, block.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Nodes[block.NodeID].Status != state.NodeStatusReady || resumed.Nodes[block.TargetNodeID].Status != state.NodeStatusRunning {
		t.Fatalf("stale block silently re-blocked resolved pair: child=%#v parent=%#v", resumed.Nodes[block.NodeID], resumed.Nodes[block.TargetNodeID])
	}
	if resumed.LastLogSeq != resolved.LastLogSeq+1 {
		t.Fatalf("stale resume should append only command_observed: resolved seq %d, resumed seq %d", resolved.LastLogSeq, resumed.LastLogSeq)
	}
	assertRunVerifies(t, fs, snapshot.Run.ID)
}

func TestDelayedResolutionRequestCannotResolveLaterPoison(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	if _, err := executor.Drive(t.Context(), snapshot.Run.ID); err != nil {
		t.Fatal(err)
	}
	request := blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)
	if _, err := executor.ResolveBlocked(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	reblocked, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := reblocked.State.Nodes["work.test.tests"]
	if gate.Status != state.NodeStatusBlocked || gate.BlockedAttempt != 2 {
		t.Fatalf("fresh retry did not reach a later poison generation: %#v", gate)
	}
	if _, err := executor.ResolveBlocked(t.Context(), request); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("delayed attempt-1 request resolved attempt-2 poison: %v", err)
	}
	latest, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State.Nodes["work.test.tests"].Status != state.NodeStatusBlocked || latest.State.Nodes["work"].Status != state.NodeStatusBlocked {
		t.Fatalf("stale request changed later blocked pair: child=%#v parent=%#v", latest.State.Nodes["work.test.tests"], latest.State.Nodes["work"])
	}
}

func TestStaleDecisionResolutionCommandCannotResolveLaterPoison(t *testing.T) {
	fs, snapshot := escalationExecutorFixture(t)
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
		model.PerformerHuman: &fakeAdapter{observation: Observation{
			Actor: "human:operator", Verdict: "retry", EvidenceRef: "human-message:auto-retry",
		}},
	})
	block, _ := driveUntilBlockCommand(t, executor, fs, snapshot.Run.ID)
	if _, err := executor.Execute(t.Context(), block); err != nil {
		t.Fatal(err)
	}
	blocked, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	command := plan.Command{
		ID: "cmd_0123456789abcdef01234567", IdempotencyKey: snapshot.Run.ID + "/resolve_block/escalate/work/attempt-1/retry",
		Kind: plan.CommandKindResolveBlock, RunID: snapshot.Run.ID, NodeID: "escalate", TargetNodeID: "work",
		PoisonedNodeID: "work.test.tests", BlockedAttempt: 1, BlockDecision: state.BlockDecisionRetry, Actor: "human:operator",
		Reason: "escalation decision chose retry", EvidenceRef: "human-message:42:reply",
	}
	claimed, _, err := executor.claim(t.Context(), blocked, command)
	if err != nil || !claimed {
		t.Fatalf("claim decision resolution = %v, err = %v", claimed, err)
	}
	if _, err := executor.ResolveBlocked(t.Context(), blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)); err != nil {
		t.Fatal(err)
	}

	// Drive only the fresh gate-attempt commands, leaving the deliberately
	// delayed resolve command issued until attempt 2 poisons the node again.
	for round := 0; round < 20; round++ {
		latest, loadErr := fs.LoadRun(t.Context(), snapshot.Run.ID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		gate := latest.State.Nodes["work.test.tests"]
		if gate.Status == state.NodeStatusBlocked && gate.BlockedAttempt == 2 {
			break
		}
		commands, planErr := plan.Plan(latest.State, mustTemplate(t, fs, latest.Run.TemplateRef))
		if planErr != nil {
			t.Fatal(planErr)
		}
		if len(commands) == 0 {
			t.Fatal("later poison made no progress")
		}
		for _, next := range commands {
			if next.ID == command.ID {
				continue
			}
			if _, executeErr := executor.Execute(t.Context(), next); executeErr != nil {
				t.Fatal(executeErr)
			}
		}
	}
	latest, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gate := latest.State.Nodes["work.test.tests"]; gate.Status != state.NodeStatusBlocked || gate.BlockedAttempt != 2 {
		t.Fatalf("later poison = %#v", gate)
	}
	resumed, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, command.ID)
	if err != nil {
		t.Fatalf("superseded decision command wedged recovery: %v", err)
	}
	if resumed.OutstandingCommands[command.ID].Status != state.CommandStatusObserved || resumed.Nodes["work"].Status != state.NodeStatusBlocked {
		t.Fatalf("superseded command changed later poison or stayed issued: command=%#v parent=%#v", resumed.OutstandingCommands[command.ID], resumed.Nodes["work"])
	}
	current := blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)
	current.BlockedAttempt = 2
	if _, err := executor.ResolveBlocked(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Drive(t.Context(), snapshot.Run.ID); err != nil {
		t.Fatalf("run could not drive after stale command closure: %v", err)
	}
}

func TestClaimedResolutionAcceptsEquivalentManualProvenance(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	blocked, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	command := plan.Command{
		ID: "cmd_equivalent_resolution", IdempotencyKey: snapshot.Run.ID + "/resolve/equivalent",
		Kind: plan.CommandKindResolveBlock, RunID: snapshot.Run.ID, NodeID: "work", TargetNodeID: "work",
		PoisonedNodeID: "work.test.tests", BlockedAttempt: 1, BlockDecision: state.BlockDecisionRetry,
		Actor: "human:reviewer", Reason: "decision selected retry", EvidenceRef: "human-message:decision",
	}
	if claimed, _, err := executor.claim(t.Context(), blocked, command); err != nil || !claimed {
		t.Fatalf("claim resolution = %v, err = %v", claimed, err)
	}
	if _, err := executor.ResolveBlocked(t.Context(), blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)); err != nil {
		t.Fatal(err)
	}
	resumed, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, command.ID)
	if err != nil {
		t.Fatalf("equivalent manual resolution wedged command: %v", err)
	}
	if got := resumed.OutstandingCommands[command.ID]; got.Status != state.CommandStatusObserved || got.Verdict != "pass" {
		t.Fatalf("equivalent command was not closed as pass: %#v", got)
	}
}

func TestClaimedResolutionClosesAfterContradictingManualDecision(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	blocked, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	command := plan.Command{
		ID: "cmd_superseded_resolution", IdempotencyKey: snapshot.Run.ID + "/resolve/superseded",
		Kind: plan.CommandKindResolveBlock, RunID: snapshot.Run.ID, NodeID: "work", TargetNodeID: "work",
		PoisonedNodeID: "work.test.tests", BlockedAttempt: 1, BlockDecision: state.BlockDecisionRetry,
		Actor: "human:reviewer", Reason: "decision selected retry", EvidenceRef: "human-message:decision",
	}
	if claimed, _, err := executor.claim(t.Context(), blocked, command); err != nil || !claimed {
		t.Fatalf("claim resolution = %v, err = %v", claimed, err)
	}
	if _, err := executor.ResolveBlocked(t.Context(), blockRequest(snapshot.Run.ID, state.BlockDecisionCancel)); err != nil {
		t.Fatal(err)
	}
	resumed, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, command.ID)
	if err != nil {
		t.Fatalf("contradicted manual resolution wedged command: %v", err)
	}
	if got := resumed.OutstandingCommands[command.ID]; got.Status != state.CommandStatusObserved || got.Verdict != "superseded" {
		t.Fatalf("contradicted command was not explicitly superseded: %#v", got)
	}
	if resumed.Status != state.RunStatusCanceled {
		t.Fatalf("superseded retry changed manual cancel: %s", resumed.Status)
	}
}

func TestResumeStalePoisonEscalationActivationIsNoOp(t *testing.T) {
	fs, snapshot := escalationExecutorFixture(t)
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	block, _ := driveUntilBlockCommand(t, executor, fs, snapshot.Run.ID)
	if _, err := executor.Execute(t.Context(), block); err != nil {
		t.Fatal(err)
	}
	blocked, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := plan.Plan(blocked.State, mustTemplate(t, fs, blocked.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Kind != plan.CommandKindActivateNode || commands[0].TargetNodeID != "escalate" {
		t.Fatalf("escalation activation = %#v", commands)
	}
	activation := commands[0]
	claimed, _, err := executor.claim(t.Context(), blocked, activation)
	if err != nil || !claimed {
		t.Fatalf("claim escalation activation = %v, err = %v", claimed, err)
	}
	if _, err := executor.ResolveBlocked(t.Context(), blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)); err != nil {
		t.Fatal(err)
	}
	resumed, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, activation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Nodes["escalate"].Status != state.NodeStatusPending {
		t.Fatalf("stale activation created obsolete decision: %#v", resumed.Nodes["escalate"])
	}
	if resumed.OutstandingCommands[activation.ID].Status != state.CommandStatusObserved {
		t.Fatalf("stale activation not closed: %#v", resumed.OutstandingCommands[activation.ID])
	}
	assertRunVerifies(t, fs, snapshot.Run.ID)
}

func escalationExecutorFixture(t *testing.T) (*store.FS, store.Snapshot) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	program := func(script string) *model.Performer {
		return &model.Performer{Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", script}}
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-escalation", Start: "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask, Performer: program("exit 0"),
				Checks: []model.Step{{ID: "tests", Performer: *program("exit 1")}},
				Next:   model.Next{"pass": "end", "fail": "escalate"},
			},
			"escalate": {
				Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Continue?"},
				Next: model.Next{"retry": "work", "cancel": "canceled"},
			},
			"end":      {Type: model.NodeTypeEnd},
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_escalation"
	initial := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "escalate", Type: model.NodeTypeDecision, Status: state.NodeStatusPending},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "canceled", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{{
		At: executorTestTime, Scope: evidence.Scope{Kind: evidence.ScopeRun}, Kind: evidence.EntryKindAdmin,
		Event: &state.Event{Type: state.EventAdminProgramsAllowed, At: executorTestTime, Actor: "human:test", Reason: "test program opt-in"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetProgramsAllowed(t.Context(), runID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return fs, snapshot
}

func TestBindBlockResolutionPinsParentSelectionToChildGeneration(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	blocked, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := blockRequest(snapshot.Run.ID, state.BlockDecisionSkip)
	request.NodeID = "work"
	request.BlockedAttempt = 0
	bound, err := BindBlockResolution(blocked, request)
	if err != nil {
		t.Fatal(err)
	}
	if bound.NodeID != "work.test.tests" || bound.BlockedAttempt != 1 {
		t.Fatalf("bound request = %#v", bound)
	}
	if _, err := executor.ResolveBlocked(t.Context(), bound); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeBlockResolutionRejectsInvalidInputs(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	if _, err := executor.Drive(t.Context(), snapshot.Run.ID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*BlockResolutionRequest)
		want   string
	}{
		{name: "decision", mutate: func(r *BlockResolutionRequest) { r.Decision = "bogus" }, want: "retry, skip, or cancel"},
		{name: "actor", mutate: func(r *BlockResolutionRequest) { r.Actor = "engine:forged" }, want: "non-engine actor"},
		{name: "reason", mutate: func(r *BlockResolutionRequest) { r.Reason = "" }, want: "reason is required"},
		{name: "evidence", mutate: func(r *BlockResolutionRequest) { r.EvidenceRef = "" }, want: "evidence ref is required"},
		{name: "negative attempt", mutate: func(r *BlockResolutionRequest) { r.BlockedAttempt = -1 }, want: "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := blockRequest(snapshot.Run.ID, state.BlockDecisionRetry)
			tt.mutate(&request)
			if _, err := executor.ResolveBlocked(t.Context(), request); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func blockRequest(runID string, decision state.BlockDecision) BlockResolutionRequest {
	return BlockResolutionRequest{
		RunID: runID, NodeID: "work.test.tests", BlockedAttempt: 1, Decision: decision,
		Actor: "human:operator", Reason: "operator reviewed poison", EvidenceRef: "decision:TCL-279",
	}
}

func assertResolvedPair(t *testing.T, st *state.State, childStatus state.NodeStatus, runStatus state.RunStatus, decision state.BlockDecision) {
	t.Helper()
	child, parent := st.Nodes["work.test.tests"], st.Nodes["work"]
	if child.Status != childStatus || parent.Status != state.NodeStatusRunning || st.Status != runStatus {
		t.Fatalf("resolved pair: run=%s child=%#v parent=%#v", st.Status, child, parent)
	}
	for nodeID, node := range map[string]state.NodeState{"work.test.tests": child, "work": parent} {
		if node.BlockedReason != "" || node.BlockedOwner != "" || node.BlockResolution == nil || node.BlockResolution.Decision != decision {
			t.Fatalf("resolved node %s = %#v", nodeID, node)
		}
		if node.BlockedAt.IsZero() {
			t.Fatalf("resolved node %s lost blockedAt: %#v", nodeID, node)
		}
	}
	contactFound := false
	for commandID, command := range st.OutstandingCommands {
		if command.Kind != state.CommandKindBlockNode || command.NodeID != "work.test.tests" {
			continue
		}
		contact := st.Contacts[commandID]
		contactFound = true
		if !contact.Paused || contact.PauseReason != "block resolved" || !contact.NextContactAt.IsZero() {
			t.Fatalf("resolved block contact remains active: %#v", contact)
		}
	}
	if !contactFound {
		t.Fatal("resolved block has no retained contact history")
	}
	if len(st.AdminRecords) == 0 || st.AdminRecords[len(st.AdminRecords)-1].Type != state.EventBlockResolutionRecorded {
		t.Fatalf("resolution audit missing: %#v", st.AdminRecords)
	}
}

func assertRunVerifies(t *testing.T, fs store.Store, runID string) {
	t.Helper()
	if report := processverify.StoreRun(t.Context(), fs, runID); report.HasErrors() {
		t.Fatalf("verify %s: %#v", runID, report.Diagnostics)
	}
}

func driveUntilBlockCommand(t *testing.T, executor *Executor, fs store.Store, runID string) (plan.Command, store.Snapshot) {
	t.Helper()
	for round := 0; round < 50; round++ {
		snapshot, err := fs.LoadRun(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
		if err != nil {
			t.Fatal(err)
		}
		for _, command := range commands {
			if command.Kind == plan.CommandKindBlockNode {
				return command, snapshot
			}
			if _, err := executor.Execute(t.Context(), command); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("block command was not planned")
	return plan.Command{}, store.Snapshot{}
}
