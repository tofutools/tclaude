package executor

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// createApprovalRun persists start -> build -> end where build is a compound
// whose plan stage needs a human approval. Every stage program succeeds, so the
// only thing that can ever hold this run is the person at the gate.
func createApprovalRun(t *testing.T, runID string, work engine.ProgramCommand) {
	t.Helper()
	performer := model.Performer{
		Kind: model.PerformerProgram, Profile: work.Profile,
		Run: work.Run, Args: append([]string(nil), work.Args...),
	}
	doPerformer := performer
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-approval-test", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "build"}},
			"build": {
				Type:      model.NodeTypeTask,
				Performer: &doPerformer,
				Plan:      &model.Step{ID: "plan", Performer: performer, Approval: model.PlanApprovalHuman},
				Next:      model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	})
}

// runOneStage drives exactly one dispatched program to its durable observation.
func runOneStage(t *testing.T, run *Run, wantNodeID string) {
	t.Helper()
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch, "expected a dispatch for %q", wantNodeID)
	require.Equal(t, wantNodeID, dispatch.Command().NodeID)
	_, _, err = executeForTest(t.Context(), run, dispatch,
		Authorization{RunID: run.ID(), Profile: dispatch.Command().Program.Profile})
	require.NoError(t, err)
}

// awaitingApproval asserts the run is parked on its approval gate at one exact
// window, through the same public surfaces an operator reads.
func awaitingApproval(t *testing.T, run *Run, attempt int) {
	t.Helper()
	action := run.Action()
	require.Equal(t, ActionAwaitDecision, action.Kind)
	require.NotNil(t, action.Decision)
	assert.Equal(t, "build.plan.approval", action.Decision.NodeID)
	assert.Equal(t, []string{engine.ApprovalApprove, engine.ApprovalRework},
		run.VerdictsFor("build.plan.approval"))
	got, ok := run.definition.ApprovalAttempt(run.checkpoint, "build.plan.approval")
	require.True(t, ok, "an approval gate must report its window")
	assert.Equal(t, attempt, got)
}

// TestApprovalWindowEvidenceIsOrderedAndAttributed is the evidence contract for
// one whole rework cycle: every window opening, the verdict that closed it, the
// reset a rework caused, and the plan run that reset made possible, each in the
// commit that actually caused it and in causal order.
//
// The first decision_awaited row is the regression that matters most here: the
// obligation is created BY the plan observation's own transition, which an
// advance-scoped evidence diff never saw.
func TestApprovalWindowEvidenceIsOrderedAndAttributed(t *testing.T) {
	setupExecutorTest(t)
	createApprovalRun(t, "run_approval_evidence", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_approval_evidence")

	runOneStage(t, run, "build.plan")
	awaitingApproval(t, run, 1)

	// The observation that finished the plan and the window it opened are one
	// transaction, in that order, and no engine_advanced row stands in for the
	// obligation.
	kinds := eventKinds(t, run.ID())
	assert.Equal(t, []string{"program_prepared", "program_observed", "decision_awaited"}, kinds)
	awaited := eventOfKind(t, run.ID(), "decision_awaited")
	assert.Equal(t, "build.plan.approval", awaited.NodeID)
	assert.Equal(t, EngineActor, awaited.Actor, "an obligation the run took on is the engine's")
	var payload map[string]any
	require.NoError(t, awaited.DecodePayload(&payload))
	assert.Equal(t, map[string]any{"nodeId": "build.plan.approval", "attempt": float64(1)}, payload,
		"a recurring window has to say which one it is")

	// The rework verdict and the reset it caused are one transaction, in that
	// order, and the reset names the PLAN as the work that runs again.
	require.NoError(t, RecordDecision(run, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalRework,
		Attempt: 1, Evidence: "the plan skips the migration step",
	}))
	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	recorded := slices.IndexFunc(events, func(event db.ProcessRunEvent) bool {
		return event.Kind == "decision_recorded"
	})
	require.GreaterOrEqual(t, recorded, 0)
	require.Less(t, recorded+1, len(events))
	assert.Equal(t, "build.plan.approval", events[recorded].NodeID)
	assert.Equal(t, "operator@example", events[recorded].Actor)
	assert.Equal(t, "stage_reset", events[recorded+1].Kind)
	assert.Equal(t, "build", events[recorded+1].NodeID)
	assert.Equal(t, EngineActor, events[recorded+1].Actor)

	// The verdict row carries the window and the human's evidence, and invents no
	// authored edge for a gate that has none.
	var verdict map[string]any
	require.NoError(t, events[recorded].DecodePayload(&verdict))
	assert.Equal(t, map[string]any{
		"verdict": engine.ApprovalRework, "attempt": float64(1),
		"evidence": "the plan skips the migration step",
	}, verdict)

	var reset map[string]any
	require.NoError(t, events[recorded+1].DecodePayload(&reset))
	assert.Equal(t, map[string]any{
		"parentNodeId": "build", "gateNodeId": "build.plan.approval",
		"workNodeId": "build.plan", "nextWorkAttempt": float64(2),
	}, reset)

	// The second plan run reopens the window at its own attempt, recorded the
	// same way, and approving it steps the compound on to its do stage.
	runOneStage(t, run, "build.plan")
	awaitingApproval(t, run, 2)
	reopened := eventsOfKind(t, run.ID(), "decision_awaited")
	require.Len(t, reopened, 2, "every window opening is recorded")
	require.NoError(t, reopened[1].DecodePayload(&payload))
	assert.Equal(t, map[string]any{"nodeId": "build.plan.approval", "attempt": float64(2)}, payload)

	require.NoError(t, RecordDecision(run, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: 2,
	}))
	assert.Empty(t, run.AwaitingDecisions())
	// Approving causes no reset: it is a step forwards along the child list.
	assert.Equal(t, 1, countKind(eventKinds(t, run.ID()), "stage_reset"))

	runOneStage(t, run, "build.do")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.Nil(t, dispatch)
	assert.Equal(t, engine.RunCompleted, run.Action().Status)

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, 2, checkpoint.Attempts["build.plan"], "one rework buys exactly one more plan run")
	assert.Equal(t, 1, checkpoint.Attempts["build.do"])
	assert.Empty(t, checkpoint.AttemptCeilings, "a human rework writes no ceiling")
}

// TestStaleApprovalVerdictIsRefusedWithoutTouchingTheRun is the rollback half:
// every refusal leaves the state version, the checkpoint, and the evidence
// stream exactly as they were, so a delayed or duplicated human action cannot
// half-apply.
func TestStaleApprovalVerdictIsRefusedWithoutTouchingTheRun(t *testing.T) {
	setupExecutorTest(t)
	createApprovalRun(t, "run_approval_stale", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_approval_stale")
	runOneStage(t, run, "build.plan")
	require.NoError(t, RecordDecision(run, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalRework, Attempt: 1,
	}))
	runOneStage(t, run, "build.plan")
	awaitingApproval(t, run, 2)

	version := run.StateVersion()
	before := eventKinds(t, run.ID())
	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	checkpointBefore := string(record.CheckpointJSON)

	for name, input := range map[string]DecisionInput{
		"previous window": {NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: 1},
		"unopened window": {NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: 3},
		"absent window":   {NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove},
		"wrong node":      {NodeID: "build.plan", Verdict: engine.ApprovalApprove, Attempt: 2},
		"wrong verdict":   {NodeID: "build.plan.approval", Verdict: "looks-fine", Attempt: 2},
	} {
		t.Run(name, func(t *testing.T) {
			err := RecordDecision(run, "operator@example", input)
			require.Error(t, err)
			if input.Verdict == "looks-fine" {
				assert.ErrorIs(t, err, engine.ErrInvalidDecisionVerdict)
			} else {
				assert.ErrorIs(t, err, engine.ErrStaleDecision)
			}
			assert.Equal(t, version, run.StateVersion(), "a refused verdict must not bump the version")
			assert.Equal(t, before, eventKinds(t, run.ID()), "a refused verdict must record nothing")
			current, err := db.GetProcessRun(run.ID())
			require.NoError(t, err)
			assert.Equal(t, checkpointBefore, string(current.CheckpointJSON))
		})
	}

	// A negative window never reaches the engine: it is bounded input.
	err = RecordDecision(run, "operator@example",
		DecisionInput{NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: -1})
	assert.ErrorIs(t, err, ErrInvalidDecisionInput)

	// The window it IS asking for still decides, so nothing above wedged the run.
	require.NoError(t, RecordDecision(run, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: 2,
	}))
	// And the same verdict replayed afterwards is refused as the duplicate it is.
	assert.ErrorIs(t, RecordDecision(run, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: 2,
	}), engine.ErrStaleDecision)
}

// TestApprovalSurvivesRestartWhileAwaitingAndMidRework is the cold-load proof.
// The window identity is derived from the plan counter and nothing else, so a
// daemon restart at either point resumes asking the same question.
func TestApprovalSurvivesRestartWhileAwaitingAndMidRework(t *testing.T) {
	setupExecutorTest(t)
	createApprovalRun(t, "run_approval_restart", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_approval_restart")
	runOneStage(t, run, "build.plan")

	// Restart while the window is open: the obligation, its verdicts, and its
	// window all come back, and a resume commits nothing.
	restarted := mustLoadRun(t, "run_approval_restart")
	version := restarted.StateVersion()
	awaitingApproval(t, restarted, 1)
	dispatch, err := Prepare(restarted)
	require.NoError(t, err)
	require.Nil(t, dispatch, "a run awaiting a human has nothing to plan")
	assert.Equal(t, version, restarted.StateVersion(), "a quiescent resume must not bump the version")

	require.NoError(t, RecordDecision(restarted, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalRework, Attempt: 1,
	}))

	// Restart mid-rework, before the next plan run: the plan is plannable again
	// on the cold load, and the reopened window is the new one.
	midRework := mustLoadRun(t, "run_approval_restart")
	assert.Empty(t, midRework.AwaitingDecisions(), "a reworked window is closed until the plan re-runs")
	runOneStage(t, midRework, "build.plan")
	awaitingApproval(t, midRework, 2)

	require.NoError(t, RecordDecision(midRework, "operator@example", DecisionInput{
		NodeID: "build.plan.approval", Verdict: engine.ApprovalApprove, Attempt: 2,
	}))
	runOneStage(t, midRework, "build.do")
	assert.Equal(t, engine.RunCompleted, midRework.Action().Status)
}

// TestTaskSuccessActivatedDecisionRecordsItsObligation is the regression for the
// pre-existing gap this slice fixes, on the AUTHORED path rather than the new
// one: an obligation created inside a program observation's own transition used
// to be invisible to the advance-scoped evidence diff, so the run parked on a
// human with no row saying so.
func TestTaskSuccessActivatedDecisionRecordsItsObligation(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createRunFromTemplate(t, "run_task_to_decision", &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "task-then-decision", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "work"}},
			"work": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram, Profile: program.Profile,
					Run: program.Run, Args: append([]string(nil), program.Args...),
				},
				Next: model.Next{model.DefaultOutcome: "decide"},
			},
			"decide": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"},
				Next:      model.Next{"ship": "end", "hold": "canceled"},
			},
			"end":      {Type: model.NodeTypeEnd},
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	})
	run := mustLoadRun(t, "run_task_to_decision")
	runOneStage(t, run, "work")

	require.Equal(t, ActionAwaitDecision, run.Action().Kind)
	assert.Equal(t, []string{"program_prepared", "program_observed", "decision_awaited"},
		eventKinds(t, run.ID()), "the obligation the observation created must be recorded with it")

	// An authored decision opens exactly once, so its row keeps the exact shape
	// it has always had: no window field at all.
	awaited := eventOfKind(t, run.ID(), "decision_awaited")
	var payload map[string]any
	require.NoError(t, awaited.DecodePayload(&payload))
	assert.Equal(t, map[string]any{"nodeId": "decide"}, payload)

	// And it still decides with no window named, recording the authored edge.
	require.NoError(t, RecordDecision(run, "operator@example", DecisionInput{NodeID: "decide", Verdict: "ship"}))
	recorded := eventOfKind(t, run.ID(), "decision_recorded")
	var verdict map[string]any
	require.NoError(t, recorded.DecodePayload(&verdict))
	assert.Equal(t, map[string]any{
		"verdict": "ship",
		"chosenEdge": map[string]any{
			"from": "decide", "outcome": "ship", "to": "end",
		},
	}, verdict, "an authored decision's payload must stay byte-compatible")
	assert.Equal(t, 0, countKind(eventKinds(t, run.ID()), "stage_reset"))
}

// TestAwaitedEvidenceIsBudgetedAgainstTheRestOfTheCommit keeps the newly
// centralized obligation rows inside the store's per-transaction limit: they are
// subtracted from what join history may spend, exactly as the other groups are,
// so the optional part still yields rather than the transaction being refused.
func TestAwaitedEvidenceIsBudgetedAgainstTheRestOfTheCommit(t *testing.T) {
	setupExecutorTest(t)
	definition, err := engine.Prepare(wideMixedJoinAnyTemplate(33, 224), map[string]string{})
	require.NoError(t, err)
	before, err := engine.Initialize("run_awaited_budget", definition)
	require.NoError(t, err)
	after, _, _, err := engine.AdvanceAndPlan(before, definition)
	require.NoError(t, err)

	events := commitEvidence(definition, before, after, nil, advanceEvidence(nil), true)
	require.LessOrEqual(t, len(events), db.MaxProcessRunEventsPerTransition)
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Kind]++
	}
	assert.Equal(t, 224, counts["decision_awaited"], "every parked branch keeps its own row")
	assert.Equal(t, 1, counts["join_arrivals_truncated"], "join history is what yields")
	assert.Equal(t, 0, counts["engine_advanced"], "an advance that opened windows is not silent")

	// Ordering: the arrivals that settled precede the obligations the same pass
	// parked, which precede whatever the advance then prepared.
	firstAwaited := slices.IndexFunc(events, func(event db.ProcessRunEvent) bool {
		return event.Kind == "decision_awaited"
	})
	lastJoin := -1
	for index, event := range events {
		if event.Kind == "join_won" || event.Kind == "join_arrival_late" {
			lastJoin = index
		}
	}
	require.GreaterOrEqual(t, firstAwaited, 0)
	require.GreaterOrEqual(t, lastJoin, 0)
	assert.Less(t, lastJoin, firstAwaited)

	// An advance that neither opened a window nor prepared a command still leaves
	// its plain status row, so a commit is never silently empty.
	quiet := commitEvidence(definition, after, after, nil, advanceEvidence(nil), true)
	require.Len(t, quiet, 1)
	assert.Equal(t, "engine_advanced", quiet[0].Kind)
	assert.Empty(t, commitEvidence(definition, after, after, nil, nil, false),
		"an external input that settled nothing records only what its caller passed")
}

// eventsOfKind returns every evidence row of one kind, in commit order.
func eventsOfKind(t *testing.T, runID, kind string) []db.ProcessRunEvent {
	t.Helper()
	events, err := db.ListProcessRunEvents(runID, 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	var matching []db.ProcessRunEvent
	for _, event := range events {
		if event.Kind == kind {
			matching = append(matching, event)
		}
	}
	return matching
}

// TestLoadRunRefusesAnApprovalObligationWithoutAPlanAttempt is the cold-load
// half of the fail-closed rule: a run whose approval window has no identity is
// refused at reconstruction rather than resumed and decided.
func TestLoadRunRefusesAnApprovalObligationWithoutAPlanAttempt(t *testing.T) {
	setupExecutorTest(t)
	createApprovalRun(t, "run_approval_no_attempt", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_approval_no_attempt")
	runOneStage(t, run, "build.plan")
	awaitingApproval(t, run, 1)

	// Rewrite the durable checkpoint with the one counter the window is derived
	// from removed, leaving everything else exactly as the run left it.
	record, err := db.GetProcessRun("run_approval_no_attempt")
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	require.Equal(t, 1, checkpoint.Attempts["build.plan"])
	delete(checkpoint.Attempts, "build.plan")
	encoded, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	_, err = db.TransitionProcessRun("run_approval_no_attempt", db.ProcessRunTransition{
		ExpectedStateVersion: record.StateVersion,
		Status:               record.Status,
		CheckpointJSON:       encoded,
	})
	require.NoError(t, err)

	_, err = LoadRun("run_approval_no_attempt")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRun)
	// The load boundary flattens the engine's error into its text rather than
	// wrapping the sentinel, so the reason is asserted on the message.
	assert.Contains(t, err.Error(), "has no recorded plan attempt")
}
