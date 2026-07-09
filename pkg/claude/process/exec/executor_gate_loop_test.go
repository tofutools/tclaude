package processexec

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

// scriptedAdapter plays back per-node observation queues. The program adapter
// cannot exercise the evidence-unchanged short-circuit (its evidence artifact
// embeds timestamps, so hashes always differ); scripting the observations
// pins the evidence hashes the loop compares.
type scriptedAdapter struct {
	script   map[string][]Observation
	requests []Request
}

func (a *scriptedAdapter) Validate(Request) error { return nil }

func (a *scriptedAdapter) Perform(_ context.Context, request Request) (Observation, error) {
	a.requests = append(a.requests, request)
	queue := a.script[request.Input.NodeID]
	if len(queue) == 0 {
		return Observation{}, fmt.Errorf("no scripted observation left for %s", request.Input.NodeID)
	}
	next := queue[0]
	a.script[request.Input.NodeID] = queue[1:]
	return next, nil
}

func (a *scriptedAdapter) performsFor(nodeID string) int {
	count := 0
	for _, request := range a.requests {
		if request.Input.NodeID == nodeID {
			count++
		}
	}
	return count
}

func pass(hash, ref string) Observation {
	return Observation{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: ref, EvidenceHash: hash}
}

func fail(feedback, ref string) Observation {
	return Observation{Actor: "agent:agt_test1", Verdict: "fail", Feedback: feedback, EvidenceRef: ref}
}

// gateLoopExecutorFixture stages an agent-performer compound node with a do
// stage, one check gate, and a review gate, each with an explicit budget.
func gateLoopExecutorFixture(t *testing.T, workRetry, testsRetry, reviewRetry int) (*store.FS, store.Snapshot) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := func(prompt string) model.Performer {
		return model.Performer{Kind: model.PerformerAgent, Prompt: prompt}
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "executor-gate-loop",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {
				Type:      model.NodeTypeTask,
				Performer: new(agent("do the work")),
				Checks:    []model.Step{{ID: "tests", Retry: &model.RetryPolicy{MaxAttempts: testsRetry}, Performer: agent("run the tests")}},
				Review:    &model.Step{ID: "review", Retry: &model.RetryPolicy{MaxAttempts: reviewRetry}, Performer: agent("review the diff")},
				Retry:     &model.RetryPolicy{MaxAttempts: workRetry},
				Next:      model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_gate_loop"
	initial := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return fs, snapshot
}

func TestDriveGateFeedbackLoopToCompletion(t *testing.T) {
	fs, snapshot := gateLoopExecutorFixture(t, 3, 3, 2)
	adapter := &scriptedAdapter{script: map[string][]Observation{
		"work.do":         {pass("hash-a", "e-do-1"), pass("hash-b", "e-do-2"), pass("hash-c", "e-do-3")},
		"work.test.tests": {fail("TestFoo fails", "e-t-1"), pass("hash-b", "e-t-2"), pass("hash-c", "e-t-3")},
		"work.review":     {fail("needs polish", "e-r-1"), pass("hash-c", "e-r-2")},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted {
		t.Fatalf("finished status = %s\nstate = %#v", finished.State.Status, finished.State.Nodes)
	}

	// Two loop windows: tests failed once (re-entered do), review failed once
	// (re-entered do and reset the tests gate), and every re-run evaluated
	// fresh evidence, so all three tests verdicts came from real performs.
	if got := adapter.performsFor("work.test.tests"); got != 3 {
		t.Fatalf("tests gate performs = %d, want 3", got)
	}
	gate := finished.State.Nodes["work.test.tests"]
	if len(gate.Decisions) != 3 {
		t.Fatalf("tests decisions = %#v", gate.Decisions)
	}
	// The review-triggered reset zeroed the cross-kind tests counter; the
	// review gate kept its own counter.
	if gate.FailCount != 0 {
		t.Fatalf("tests fail count = %d", gate.FailCount)
	}
	if review := finished.State.Nodes["work.review"]; review.FailCount != 1 || len(review.Decisions) != 2 {
		t.Fatalf("review = %#v", review)
	}
	if work := finished.State.Nodes["work.do"]; work.Attempt != 3 || work.PendingFeedback != nil {
		t.Fatalf("do stage = %#v", work)
	}

	// Feedback and retry mode are adapter-visible on the do stage's re-entry
	// attempts.
	var doCommands []plan.Command
	for _, request := range adapter.requests {
		if request.Input.NodeID == "work.do" {
			doCommands = append(doCommands, request.Command)
		}
	}
	if len(doCommands) != 3 {
		t.Fatalf("do performs = %d", len(doCommands))
	}
	if doCommands[0].Feedback != "" || doCommands[0].RetryMode != model.DefaultRetryMode {
		t.Fatalf("first do command = %#v", doCommands[0])
	}
	if doCommands[1].Feedback != "TestFoo fails" || doCommands[1].FeedbackFrom != "work.test.tests" {
		t.Fatalf("second do command = %#v", doCommands[1])
	}
	if doCommands[2].Feedback != "needs polish" || doCommands[2].FeedbackFrom != "work.review" {
		t.Fatalf("third do command = %#v", doCommands[2])
	}

	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify after loop: %#v", report.Diagnostics)
	}
}

func TestDriveShortCircuitsUnchangedEvidenceAndPoisons(t *testing.T) {
	// The review gate rejects hash-b work; the do stage re-runs but produces
	// byte-identical evidence. Both span gates then short-circuit — the tests
	// gate stands its pass without re-running, and the review gate stands its
	// FAIL, burning its budget — so the loop cannot spin on no-op rework and
	// the node poisons.
	fs, snapshot := gateLoopExecutorFixture(t, 3, 3, 2)
	adapter := &scriptedAdapter{script: map[string][]Observation{
		"work.do":         {pass("hash-a", "e-do-1"), pass("hash-b", "e-do-2"), pass("hash-b", "e-do-3")},
		"work.test.tests": {fail("TestFoo fails", "e-t-1"), pass("hash-b", "e-t-2")},
		"work.review":     {fail("needs polish", "e-r-1")},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusRunning {
		t.Fatalf("finished status = %s", finished.State.Status)
	}

	// The third tests verdict is an engine short-circuit standing the pass.
	if got := adapter.performsFor("work.test.tests"); got != 2 {
		t.Fatalf("tests gate performs = %d, want 2 (third run must short-circuit)", got)
	}
	gate := finished.State.Nodes["work.test.tests"]
	if len(gate.Decisions) != 3 {
		t.Fatalf("tests decisions = %#v", gate.Decisions)
	}
	testsEngine := gate.Decisions[2]
	if testsEngine.Actor != state.ActorEvidenceUnchanged || testsEngine.Verdict != "pass" || testsEngine.EvidenceRef != gate.Decisions[1].EvidenceRef {
		t.Fatalf("tests engine decision = %#v", testsEngine)
	}
	if gate.Status != state.NodeStatusCompleted {
		t.Fatalf("tests gate = %#v", gate)
	}

	// The review gate short-circuited its fail without a second perform and
	// exhausted its budget of 2 failed verdicts.
	if got := adapter.performsFor("work.review"); got != 1 {
		t.Fatalf("review performs = %d, want 1", got)
	}
	review := finished.State.Nodes["work.review"]
	if review.Status != state.NodeStatusBlocked || review.FailCount != 2 || len(review.Decisions) != 2 {
		t.Fatalf("review = %#v", review)
	}
	reviewEngine := review.Decisions[1]
	if reviewEngine.Actor != state.ActorEvidenceUnchanged || reviewEngine.Verdict != "fail" || reviewEngine.EvidenceRef != review.Decisions[0].EvidenceRef {
		t.Fatalf("review engine decision = %#v", reviewEngine)
	}
	if !strings.Contains(review.BlockedReason, `gate "work.review" exhausted its budget of 2 failed verdicts`) {
		t.Fatalf("blocked reason = %q", review.BlockedReason)
	}
	if finished.State.Nodes["work"].Status != state.NodeStatusBlocked {
		t.Fatalf("parent = %#v", finished.State.Nodes["work"])
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify after short-circuit poison: %#v", report.Diagnostics)
	}
}

func TestDriveGateBudgetExhaustionPoisons(t *testing.T) {
	fs, snapshot := gateLoopExecutorFixture(t, 3, 2, 2)
	adapter := &scriptedAdapter{script: map[string][]Observation{
		"work.do":         {pass("hash-1", "e-do-1"), pass("hash-2", "e-do-2")},
		"work.test.tests": {fail("broken", "e-t-1"), fail("still broken", "e-t-2")},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusRunning {
		t.Fatalf("finished status = %s", finished.State.Status)
	}
	gate := finished.State.Nodes["work.test.tests"]
	parent := finished.State.Nodes["work"]
	if gate.Status != state.NodeStatusBlocked || parent.Status != state.NodeStatusBlocked {
		t.Fatalf("gate = %#v, parent = %#v", gate, parent)
	}
	if !strings.Contains(gate.BlockedReason, `gate "work.test.tests" exhausted its budget of 2 failed verdicts`) {
		t.Fatalf("blocked reason = %q", gate.BlockedReason)
	}
	if gate.FailCount != 2 {
		t.Fatalf("fail count = %d", gate.FailCount)
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify after poison: %#v", report.Diagnostics)
	}
}

func TestDriveWorkBudgetBoundsFeedbackLoop(t *testing.T) {
	// The tests gate has verdicts left, but the do stage only gets one
	// attempt, so the first gate failure cannot re-enter and poisons.
	fs, snapshot := gateLoopExecutorFixture(t, 1, 3, 2)
	adapter := &scriptedAdapter{script: map[string][]Observation{
		"work.do":         {pass("hash-1", "e-do-1")},
		"work.test.tests": {fail("broken", "e-t-1")},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := finished.State.Nodes["work.test.tests"]
	if gate.Status != state.NodeStatusBlocked {
		t.Fatalf("gate = %#v", gate)
	}
	if !strings.Contains(gate.BlockedReason, `stage "work.do" has exhausted its budget of 1 attempts`) {
		t.Fatalf("blocked reason = %q", gate.BlockedReason)
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify: %#v", report.Diagnostics)
	}
}

// driveUntilPlanned plans and executes one round at a time until the planner
// emits a command of the wanted kind, and returns that command unexecuted.
func driveUntilPlanned(t *testing.T, executor *Executor, fs *store.FS, runID string, kind plan.CommandKind) plan.Command {
	t.Helper()
	for range 60 {
		snapshot, err := fs.LoadRun(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
		if err != nil {
			t.Fatal(err)
		}
		if len(commands) == 0 {
			t.Fatalf("run quiesced before a %s command was planned", kind)
		}
		for _, command := range commands {
			if command.Kind == kind {
				return command
			}
		}
		for _, command := range commands {
			if _, err := executor.Execute(t.Context(), command); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatalf("command kind %s never planned", kind)
	return plan.Command{}
}

func TestDriveResumesStaleGateFeedbackAfterManualReentry(t *testing.T) {
	// Wedge regression, same shape as the stale-expand guard: the executor
	// claims gate_feedback and crashes before observing it; a manual advance
	// lands the loop re-entry directly. The stale resumed command must treat
	// the already-re-entered loop as idempotent success — replaying the batch
	// would re-ready the do stage mid-flight and fail the reducer forever.
	fs, snapshot := gateLoopExecutorFixture(t, 3, 2, 2)
	adapter := &scriptedAdapter{script: map[string][]Observation{
		"work.do":         {pass("hash-1", "e-do-1"), pass("hash-2", "e-do-2")},
		"work.test.tests": {fail("broken", "e-t-1"), fail("still broken", "e-t-2")},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	feedback := driveUntilPlanned(t, executor, fs, snapshot.Run.ID, plan.CommandKindGateFeedback)
	current, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, claimState, err := executor.claim(t.Context(), current, feedback)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err = %v", claimed, err)
	}

	// Manual re-entry lands the same batch while the command is still issued.
	at := executorTestTime
	if _, err := fs.Append(t.Context(), snapshot.Run.ID, claimState.LastLogSeq, []evidence.LogEntry{
		nodeEntry(feedback.TargetNodeID, state.Event{
			Type:        state.EventFeedbackRecorded,
			FromNodeID:  feedback.NodeID,
			Feedback:    feedback.Feedback,
			EvidenceRef: feedback.EvidenceRef,
		}, "", at),
		nodeEntry("work", state.Event{
			Type:          state.EventGateLoopReset,
			Gates:         feedback.Gates,
			ResetCounters: feedback.ResetCounters,
			Reason:        feedback.Reason,
		}, "", at),
		nodeEntry(feedback.TargetNodeID, state.Event{
			Type:       state.EventNodeStatusSet,
			NodeStatus: state.NodeStatusReady,
		}, "", at),
	}); err != nil {
		t.Fatal(err)
	}

	// Drive finishes the loop (second gate failure exhausts the budget and
	// poisons) and resumes the stale feedback command as a no-op on the way.
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatalf("Drive must not wedge on the stale gate_feedback: %v", err)
	}
	if finished.State.Nodes["work.test.tests"].Status != state.NodeStatusBlocked {
		t.Fatalf("gate = %#v", finished.State.Nodes["work.test.tests"])
	}
	if cmd := finished.State.OutstandingCommands[feedback.ID]; cmd.Status != state.CommandStatusObserved {
		t.Fatalf("stale feedback command = %#v", cmd)
	}
	// The do stage ran exactly twice: the stale resume did not re-ready it.
	if finished.State.Nodes["work.do"].Attempt != 2 {
		t.Fatalf("do attempts = %d", finished.State.Nodes["work.do"].Attempt)
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify after recovered drive: %#v", report.Diagnostics)
	}
}

func TestResumeStaleShortCircuitAfterManualGateAdvance(t *testing.T) {
	// The executor claims short_circuit_gate and crashes; a human manually
	// advances the re-entered gate before recovery. The stale resume must be
	// an idempotent no-op — replaying gate_short_circuited against a settled
	// gate fails the reducer forever.
	fs, snapshot := gateLoopExecutorFixture(t, 3, 3, 2)
	adapter := &scriptedAdapter{script: map[string][]Observation{
		// Identical do evidence hashes on both attempts force the planner to
		// short-circuit the re-entered tests gate.
		"work.do":         {pass("hash-1", "e-do-1"), pass("hash-1", "e-do-2")},
		"work.test.tests": {fail("broken", "e-t-1")},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	shortCircuit := driveUntilPlanned(t, executor, fs, snapshot.Run.ID, plan.CommandKindShortCircuit)
	current, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, claimState, err := executor.claim(t.Context(), current, shortCircuit)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err = %v", claimed, err)
	}

	// A manual advance settles the gate while the command is still issued.
	at := executorTestTime
	gateID := shortCircuit.NodeID
	if _, err := fs.Append(t.Context(), snapshot.Run.ID, claimState.LastLogSeq, []evidence.LogEntry{
		nodeEntry(gateID, state.Event{
			Type:  state.EventNodeAttemptStarted,
			Actor: "human:johan",
		}, "", at),
		nodeEntry(gateID, state.Event{
			Type:             state.EventNodeAttemptSettled,
			Outcome:          "pass",
			NodeStatus:       state.NodeStatusCompleted,
			EvidenceRef:      "manual:re-checked",
			WorkEvidenceHash: "hash-1",
		}, "manual:re-checked", at),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, shortCircuit.ID); err != nil {
		t.Fatalf("stale short-circuit resume must no-op: %v", err)
	}
	resumed, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cmd := resumed.State.OutstandingCommands[shortCircuit.ID]; cmd.Status != state.CommandStatusObserved {
		t.Fatalf("stale short-circuit command = %#v", cmd)
	}
	gate := resumed.State.Nodes[gateID]
	if gate.Status != state.NodeStatusCompleted || len(gate.Decisions) != 2 {
		t.Fatalf("gate after manual advance + stale resume = %#v", gate)
	}
	for _, decision := range gate.Decisions {
		if strings.HasPrefix(string(decision.Actor), "engine:") {
			t.Fatalf("stale resume must not append an engine decision: %#v", gate.Decisions)
		}
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify: %#v", report.Diagnostics)
	}
}
