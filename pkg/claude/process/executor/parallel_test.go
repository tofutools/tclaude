package executor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// createFanOutRun persists a run whose template fans out to two program tasks
// and reduces them at a join: all task:
//
//	start -> fork -{left,right}-> left/right -> join(all) -> end
func createFanOutRun(t *testing.T, runID string, program engine.ProgramCommand) {
	t.Helper()
	task := func(next string) model.Node {
		return model.Node{
			Type: model.NodeTypeTask,
			Performer: &model.Performer{
				Kind: model.PerformerProgram, Profile: program.Profile,
				Run: program.Run, Args: append([]string(nil), program.Args...), Timeout: program.Timeout,
			},
			Next: model.Next{model.DefaultOutcome: next},
		}
	}
	join := task("end")
	join.Join = model.JoinAll
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-fan-out", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  task("join"),
			"right": task("join"),
			"join":  join,
			"end":   {Type: model.NodeTypeEnd},
		},
	})
}

// createTwoDecisionRun persists a run that parks both branches of a fork on
// their own human decision at the same time.
func createTwoDecisionRun(t *testing.T, runID string) {
	t.Helper()
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-two-decisions", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"a": "decide-a", "b": "decide-b"}},
			"decide-a": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "A?"},
				Next:      model.Next{"yes": "join", "no": "join"},
			},
			"decide-b": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "B?"},
				Next:      model.Next{"yes": "join", "no": "join"},
			},
			"join": {Type: model.NodeTypeEnd, Join: model.JoinAll},
		},
	})
}

func createRunFromTemplate(t *testing.T, runID string, tmpl *model.Template) {
	t.Helper()
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize(runID, definition)
	require.NoError(t, err)
	checkpointJSON, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	hash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	require.NoError(t, db.CreateProcessRun(db.ProcessRunCreate{
		ID: runID, TemplateRef: model.TemplateRef(tmpl.ID, hash),
		TemplateSnapshotJSON: snapshot, ParamsJSON: json.RawMessage(`{}`),
		Status: string(checkpoint.Status), CheckpointJSON: checkpointJSON,
	}))
}

// TestFanOutRunDispatchesAndCompletesEveryBranchSequentially is the
// independent-deployability regression: a newly created fan-out/join: all run
// is dispatchable on the existing sequential driver, and driving it consumes one
// branch at a time until every branch has run and the join reduces them. No
// branch is dropped and nothing is left outstanding.
func TestFanOutRunDispatchesAndCompletesEveryBranchSequentially(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createFanOutRun(t, "run_fan_out", program)

	run := mustLoadRun(t, "run_fan_out")
	require.Equal(t, ActionContinue, run.Action().Kind)

	// Creation path: the first Prepare reaches a real branch dispatch.
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch, "a newly created fan-out run must become dispatchable")
	require.Equal(t, ActionDispatch, run.Action().Kind)

	// Exactly one command is ever outstanding under sequential consumption.
	var executed []string
	for dispatch != nil {
		require.Len(t, run.checkpoint.Commands, 1, "sequential consumption must keep one command in flight")
		executed = append(executed, dispatch.command.NodeID)
		_, next, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID(), Profile: program.Profile})
		require.NoError(t, err)
		dispatch = next
	}

	assert.Equal(t, []string{"left", "right", "join"}, executed,
		"every branch and then the reducer must run, in deterministic order")
	assert.Equal(t, ActionTerminal, run.Action().Kind)

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.RunCompleted, checkpoint.Status)
	assert.Empty(t, checkpoint.Commands, "a completed run must strand no command")
	assert.Empty(t, checkpoint.AwaitingDecisions)
	for nodeID, status := range checkpoint.Nodes {
		assert.Equal(t, engine.NodeDone, status, "node %q", nodeID)
	}
}

// TestFanOutColdRestartKeepsTheDurableCommandReconcilable covers the restart
// path: a cold load of a fan-out run holding one durable command is ambiguous
// exactly as in the sequential slice, so it needs explicit reconciliation and
// the recorded outcome resumes the remaining branches.
func TestFanOutColdRestartKeepsTheDurableCommandReconcilable(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createFanOutRun(t, "run_fan_out_restart", program)

	run := mustLoadRun(t, "run_fan_out_restart")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	firstNode := dispatch.command.NodeID

	// Simulate a restart: the in-memory dispatch permission is gone, and the
	// durable command cannot be assumed un-executed.
	cold := mustLoadRun(t, "run_fan_out_restart")
	require.Equal(t, ActionNeedsReconcile, cold.Action().Kind)
	require.Len(t, cold.checkpoint.Commands, 1)
	require.Equal(t, firstNode, cold.checkpoint.Commands[0].NodeID)
	_, err = Prepare(cold)
	assert.ErrorIs(t, err, ErrNeedsReconcile)

	// Reconciling that one command releases the branch and the run keeps going.
	require.NoError(t, RecordOutcome(cold, "operator", RecordedOutcome{
		Outcome: engine.ProgramSucceeded, Note: "verified out of band",
	}))
	resumed, err := Prepare(cold)
	require.NoError(t, err)
	require.NotNil(t, resumed, "the sibling branch must still be dispatchable after reconciliation")
	assert.NotEqual(t, firstNode, resumed.command.NodeID, "reconciliation must not replay the same branch")

	for resumed != nil {
		_, next, err := Execute(t.Context(), cold, resumed, Authorization{RunID: cold.ID(), Profile: program.Profile})
		require.NoError(t, err)
		resumed = next
	}
	assert.Equal(t, ActionTerminal, cold.Action().Kind)
	assert.Equal(t, engine.RunCompleted, cold.Action().Status)
}

// TestConcurrentDecisionObligationsAreIndividuallyAddressable proves each
// branch's obligation is resolved on its own identity. Presentation still
// surfaces one at a time, but recording a verdict for one branch must neither
// consume nor invalidate the other's.
func TestConcurrentDecisionObligationsAreIndividuallyAddressable(t *testing.T) {
	setupExecutorTest(t)
	createTwoDecisionRun(t, "run_two_decisions")
	run := mustLoadRun(t, "run_two_decisions")

	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.Nil(t, dispatch, "a decision-only fan-out yields no dispatch")
	require.Len(t, run.checkpoint.AwaitingDecisions, 2, "both branches must be awaited at once")

	// Both obligations exist durably; the view shows the first.
	action := run.Action()
	require.Equal(t, ActionAwaitDecision, action.Kind)
	require.NotNil(t, action.Decision)
	assert.Equal(t, "decide-a", action.Decision.NodeID)
	assert.Equal(t, []string{"no", "yes"}, run.DecisionVerdicts())

	// Address the SECOND branch directly, out of presentation order.
	require.NoError(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide-b", Verdict: "yes"}))
	require.Len(t, run.checkpoint.AwaitingDecisions, 1, "one verdict must consume only its own obligation")
	assert.Equal(t, "decide-a", run.checkpoint.AwaitingDecisions[0].NodeID)
	assert.Equal(t, ActionAwaitDecision, run.Action().Kind)

	// The consumed obligation refuses replay while the sibling stays live.
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide-b", Verdict: "no"}), engine.ErrStaleDecision)
	require.Len(t, run.checkpoint.AwaitingDecisions, 1)

	require.NoError(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide-a", Verdict: "no"}))
	assert.Empty(t, run.checkpoint.AwaitingDecisions)

	dispatch, err = Prepare(run)
	require.NoError(t, err)
	assert.Nil(t, dispatch)
	assert.Equal(t, ActionTerminal, run.Action().Kind)

	// One evidence row per obligation, then one per verdict.
	assert.Equal(t, []string{
		"decision_awaited", "decision_awaited", "decision_recorded", "decision_recorded", "engine_advanced",
	}, eventKinds(t, run.ID()))
}

// TestBranchTaskDispatchesWhileAnotherBranchAwaitsADecision is the stranding
// regression: a branch parked on a human must not stop the driver from
// dispatching a sibling branch's program task.
func TestBranchTaskDispatchesWhileAnotherBranchAwaitsADecision(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createRunFromTemplate(t, "run_mixed_branches", &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-mixed-branches", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"a": "decide-a", "b": "task-b"}},
			"decide-a": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "A?"},
				Next:      model.Next{"yes": "join", "no": "join"},
			},
			"task-b": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram, Profile: program.Profile,
					Run: program.Run, Args: append([]string(nil), program.Args...),
				},
				Next: model.Next{model.DefaultOutcome: "join"},
			},
			"join": {Type: model.NodeTypeEnd, Join: model.JoinAll},
		},
	})

	run := mustLoadRun(t, "run_mixed_branches")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch, "an awaited decision on one branch must not block the other branch")
	assert.Equal(t, "task-b", dispatch.command.NodeID)
	require.Len(t, run.checkpoint.AwaitingDecisions, 1)

	_, next, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID(), Profile: program.Profile})
	require.NoError(t, err)
	assert.Nil(t, next)
	// With its own branch settled, the run correctly reports the remaining
	// obligation rather than claiming it is terminal.
	assert.Equal(t, ActionAwaitDecision, run.Action().Kind)
	assert.Equal(t, engine.RunRunning, run.Action().Status)

	require.NoError(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide-a", Verdict: "yes"}))
	dispatch, err = Prepare(run)
	require.NoError(t, err)
	assert.Nil(t, dispatch)
	assert.Equal(t, ActionTerminal, run.Action().Kind)
	assert.Equal(t, engine.RunCompleted, run.Action().Status)
}
