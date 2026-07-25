package executor

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// createGateRun persists start -> build -> end where build is a compound with
// an authored retry budget: its plan and do stages run `work` and its single
// check gate runs `gate`. Handing the gate a failing program is what puts the
// whole rework loop under the durable executor path.
func createGateRun(t *testing.T, runID string, maxAttempts int, work, gate engine.ProgramCommand) {
	t.Helper()
	performer := func(program engine.ProgramCommand) model.Performer {
		return model.Performer{
			Kind: model.PerformerProgram, Profile: program.Profile,
			Run: program.Run, Args: append([]string(nil), program.Args...),
		}
	}
	doPerformer := performer(work)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-gate-test", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "build"}},
			"build": {
				Type:      model.NodeTypeTask,
				Performer: &doPerformer,
				Retry:     &model.RetryPolicy{MaxAttempts: maxAttempts},
				Plan:      &model.Step{ID: "plan", Performer: performer(work)},
				Checks:    []model.Step{{ID: "unit", Performer: performer(gate)}},
				Next:      model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	createRunFromTemplate(t, runID, tmpl)
}

// driveGateRun follows the owner's dispatch chain — each observation plans the
// next command itself — until the run offers nothing more.
func driveGateRun(t *testing.T, run *Run, maxSteps int) {
	t.Helper()
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	for steps := 0; dispatch != nil; steps++ {
		require.Less(t, steps, maxSteps, "the rework loop did not settle")
		_, next, err := executeForTest(t.Context(), run, dispatch,
			Authorization{RunID: run.ID(), Profile: dispatch.Command().Program.Profile})
		require.NoError(t, err)
		dispatch = next
	}
}

// TestFailedGateCommitsCompactStageResetEvidence is the evidence contract for
// the rework loop: the failed gate's observation, the reset it caused, and the
// command that reset made plannable all belong to ONE transaction and appear in
// that causal order, and the reset row carries four scalars and nothing else.
func TestFailedGateCommitsCompactStageResetEvidence(t *testing.T) {
	setupExecutorTest(t)
	createGateRun(t, "run_gate_evidence", 2,
		helperProgram(t, "success"), helperProgram(t, "unrecognized-mode"))
	run := mustLoadRun(t, "run_gate_evidence")
	driveGateRun(t, run, 8)

	// The budget bought two do executions, both rejected by the gate, so the
	// branch is parked at the gate's own exact attempt.
	require.Len(t, run.Blocked(), 1)
	assert.Equal(t, "build.test.unit", run.Blocked()[0].NodeID)
	assert.Equal(t, 2, run.BlockedAttempt("build.test.unit"))
	assert.False(t, run.Draining(), "an exhausted gate must not doom the run")

	kinds := eventKinds(t, run.ID())
	assert.Equal(t, 1, countKind(kinds, "stage_reset"), "kinds = %v", kinds)
	assert.Equal(t, 1, countKind(kinds, "node_blocked"), "kinds = %v", kinds)

	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	reset := slices.IndexFunc(events, func(event db.ProcessRunEvent) bool { return event.Kind == "stage_reset" })
	require.GreaterOrEqual(t, reset, 1, "a stage reset cannot be the first thing a run records")
	// Causal order inside the one commit: the gate's verdict, the reset it
	// caused, then the work command that reset made plannable.
	assert.Equal(t, "program_observed", events[reset-1].Kind)
	assert.Equal(t, "build.test.unit", events[reset-1].NodeID)
	require.Less(t, reset+1, len(events))
	assert.Equal(t, "program_prepared", events[reset+1].Kind)
	assert.Equal(t, "build.do", events[reset+1].NodeID)

	// The row is the compound's, attributed to the engine, and compact.
	assert.Equal(t, "build", events[reset].NodeID)
	assert.Equal(t, EngineActor, events[reset].Actor)
	var payload map[string]any
	require.NoError(t, events[reset].DecodePayload(&payload))
	assert.Equal(t, map[string]any{
		"parentNodeId": "build", "gateNodeId": "build.test.unit",
		"workNodeId": "build.do", "nextWorkAttempt": float64(2),
	}, payload, "the reset row must carry only the four scalars a reader needs")

	// The gate's own bounded output stays where it already lived, and the
	// obligation's reason is bounded independently of it.
	observed := eventOfKind(t, run.ID(), "node_blocked")
	assert.Equal(t, "build.test.unit", observed.NodeID)
	assert.LessOrEqual(t, len(run.Blocked()[0].Reason), engine.MaxBlockedReasonBytes)
	assert.NotEmpty(t, run.Blocked()[0].Reason)
}

// TestBlockedGateRetryCommitsItsStageResetWithTheResolution proves the operator
// path records the same compact fact in the resolution's own transaction: one
// audited blocked_resolved row, and one engine-attributed stage_reset beside it.
func TestBlockedGateRetryCommitsItsStageResetWithTheResolution(t *testing.T) {
	setupExecutorTest(t)
	createGateRun(t, "run_gate_resolution", 1,
		helperProgram(t, "success"), helperProgram(t, "unrecognized-mode"))
	run := mustLoadRun(t, "run_gate_resolution")
	driveGateRun(t, run, 6)
	require.Len(t, run.Blocked(), 1)
	versionBefore := run.StateVersion()

	require.NoError(t, ResolveBlocked(run, "operator@example", ResolutionInput{
		NodeID: "build.test.unit", Attempt: 1, Action: engine.ResolveRetry, Note: "flaky checker host",
	}))
	assert.Equal(t, versionBefore+1, run.StateVersion())
	assert.Empty(t, run.Blocked())

	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	reset := slices.IndexFunc(events, func(event db.ProcessRunEvent) bool { return event.Kind == "stage_reset" })
	require.GreaterOrEqual(t, reset, 1)
	assert.Equal(t, "blocked_resolved", events[reset-1].Kind, "the operator input comes first")
	assert.Equal(t, "human:operator@example", "human:"+events[reset-1].Actor)
	assert.Equal(t, EngineActor, events[reset].Actor, "the reset is the engine's own effect")
	var payload map[string]any
	require.NoError(t, events[reset].DecodePayload(&payload))
	assert.Equal(t, map[string]any{
		"parentNodeId": "build", "gateNodeId": "build.test.unit",
		"workNodeId": "build.do", "nextWorkAttempt": float64(2),
	}, payload)

	// The raise landed on the work, not on the gate, and ordinary planning mints
	// the work's next attempt.
	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, map[string]int{"build.do": 2}, checkpoint.AttemptCeilings)
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	assert.Equal(t, "build.do", dispatch.Command().NodeID)
	assert.Equal(t, 2, dispatch.Command().Attempt)
}

// TestCommitsThatReworkNothingRecordNoStageReset is the negative direction, and
// it is the one that keeps the derivation honest: a commit only records a reset
// when its own input actually caused one. A compound that passed every gate,
// and an operator resolution that passed a parked gate rather than re-running
// the work, both record nothing.
func TestCommitsThatReworkNothingRecordNoStageReset(t *testing.T) {
	setupExecutorTest(t)
	success := helperProgram(t, "success")

	// Every stage passes: plan, do, gate, done, and the run completes.
	createGateRun(t, "run_gate_clean", 2, success, success)
	clean := mustLoadRun(t, "run_gate_clean")
	driveGateRun(t, clean, 8)
	record, err := db.GetProcessRun(clean.ID())
	require.NoError(t, err)
	assert.Equal(t, string(engine.RunCompleted), record.Status)
	kinds := eventKinds(t, clean.ID())
	assert.Equal(t, 0, countKind(kinds, "stage_reset"), "a clean compound run recorded a reset: %v", kinds)

	// A skip resolution passes the gate rather than re-running the work.
	createGateRun(t, "run_gate_skip", 1, success, helperProgram(t, "unrecognized-mode"))
	skipped := mustLoadRun(t, "run_gate_skip")
	driveGateRun(t, skipped, 6)
	require.Len(t, skipped.Blocked(), 1)
	require.NoError(t, ResolveBlocked(skipped, "operator@example", ResolutionInput{
		NodeID: "build.test.unit", Attempt: 1, Action: engine.ResolveSkip,
	}))
	skippedKinds := eventKinds(t, skipped.ID())
	assert.Equal(t, 1, countKind(skippedKinds, "blocked_resolved"))
	assert.Equal(t, 0, countKind(skippedKinds, "stage_reset"),
		"skipping a gate is passing it, not reworking: %v", skippedKinds)
	skippedRecord, err := db.GetProcessRun(skipped.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, skippedRecord.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["build.test.unit"])
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["build.do"], "the skip must not have re-readied the work")
}

// TestFailedGateRollsBackWhenItsEvidenceCannotCommit keeps the reset and its
// evidence atomic in the failure direction: if the stage_reset row cannot be
// written, the reset itself does not happen either, and the gate's command is
// left exactly as reconcilable as a crash would have left it.
func TestFailedGateRollsBackWhenItsEvidenceCannotCommit(t *testing.T) {
	setupExecutorTest(t)
	createGateRun(t, "run_gate_rollback", 2,
		helperProgram(t, "success"), helperProgram(t, "unrecognized-mode"))
	run := mustLoadRun(t, "run_gate_rollback")

	// Run up to the gate, then reject the reset's evidence row.
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	for dispatch != nil && dispatch.Command().NodeID != "build.test.unit" {
		_, dispatch, err = executeForTest(t.Context(), run, dispatch,
			Authorization{RunID: run.ID(), Profile: dispatch.Command().Program.Profile})
		require.NoError(t, err)
	}
	require.NotNil(t, dispatch, "the run never reached its gate")
	versionBefore := run.StateVersion()

	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER reject_stage_reset
		BEFORE INSERT ON process_run_events WHEN NEW.kind = 'stage_reset'
		BEGIN SELECT RAISE(ABORT, 'injected evidence failure'); END`)
	require.NoError(t, err)

	result, err := performForTest(t.Context(), run, dispatch,
		Authorization{RunID: run.ID(), Profile: dispatch.Command().Program.Profile})
	require.NoError(t, err)
	require.Equal(t, engine.ProgramFailed, result.Observation.Outcome)
	_, err = Observe(run, dispatch, result)
	require.Error(t, err, "a failed evidence insert must refuse the whole transition")

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	assert.Equal(t, versionBefore, record.StateVersion)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["build.do"], "the work must not have been re-readied")
	assert.Equal(t, engine.NodeRunning, checkpoint.Nodes["build.test.unit"])
	require.Len(t, checkpoint.Commands, 1)
	assert.Equal(t, "build.test.unit", checkpoint.Commands[0].NodeID)
	assert.Equal(t, 0, countKind(eventKinds(t, run.ID()), "stage_reset"))

	// The gate's command is now explicitly reconcilable, exactly as a cold load
	// would have left it: nothing can be planned past an unaccounted entry.
	assert.Equal(t, ActionNeedsReconcile, run.Action().Kind)
	_, err = Prepare(run)
	assert.ErrorIs(t, err, ErrNeedsReconcile)
}

// TestReconciledGateFailureReworksTheCompound covers the operator
// reconciliation path into the loop: a cold-loaded gate command is ambiguous,
// and recording its failure by hand drives the same reset the worker's own
// observation would have.
func TestReconciledGateFailureReworksTheCompound(t *testing.T) {
	setupExecutorTest(t)
	createGateRun(t, "run_gate_reconcile", 2,
		helperProgram(t, "success"), helperProgram(t, "unrecognized-mode"))
	run := mustLoadRun(t, "run_gate_reconcile")

	dispatch, err := Prepare(run)
	require.NoError(t, err)
	for dispatch != nil && dispatch.Command().NodeID != "build.test.unit" {
		_, dispatch, err = executeForTest(t.Context(), run, dispatch,
			Authorization{RunID: run.ID(), Profile: dispatch.Command().Program.Profile})
		require.NoError(t, err)
	}
	require.NotNil(t, dispatch, "the run never reached its gate")

	// Restart: the outstanding gate command comes back with no live permission.
	cold := mustLoadRun(t, "run_gate_reconcile")
	require.Equal(t, ActionNeedsReconcile, cold.Action().Kind)
	_, err = Prepare(cold)
	require.ErrorIs(t, err, ErrNeedsReconcile)

	require.NoError(t, RecordOutcome(cold, "operator@example", "build.test.unit", RecordedOutcome{
		Outcome: engine.ProgramFailed, ExitCode: 1, Error: "the checker rejected it", Note: "confirmed by hand",
	}))
	record, err := db.GetProcessRun(cold.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeReady, checkpoint.Nodes["build.do"])
	assert.Equal(t, engine.NodePending, checkpoint.Nodes["build.test.unit"])

	// The reset row belongs to the reconciliation's own transaction, immediately
	// after the operator's recorded outcome, and carries the same compact fact.
	events, err := db.ListProcessRunEvents(cold.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	reset := slices.IndexFunc(events, func(event db.ProcessRunEvent) bool { return event.Kind == "stage_reset" })
	require.GreaterOrEqual(t, reset, 1)
	assert.Equal(t, 1, countKind(eventKinds(t, cold.ID()), "stage_reset"))
	assert.Equal(t, "program_outcome_recorded", events[reset-1].Kind)
	assert.Equal(t, "operator@example", events[reset-1].Actor)
	assert.Equal(t, EngineActor, events[reset].Actor, "the reset is the engine's own effect")
	var payload map[string]any
	require.NoError(t, events[reset].DecodePayload(&payload))
	assert.Equal(t, map[string]any{
		"parentNodeId": "build", "gateNodeId": "build.test.unit",
		"workNodeId": "build.do", "nextWorkAttempt": float64(2),
	}, payload)
	// The reconciled run is dispatchable again, at the work's next attempt.
	next, err := Prepare(cold)
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, "build.do", next.Command().NodeID)
	assert.Equal(t, 2, next.Command().Attempt)
}
