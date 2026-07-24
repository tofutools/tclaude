package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// These runs come from templates with no start-typed node at all: the entry is
// the fork, the task, or the decision itself. Everything below drives the
// ordinary production paths — a start node was never load-bearing for them.

// createDirectParallelEntryRun reproduces the deployed TCL-649 validation
// template, minus the explicit start node it had to be given to run:
//
//	start: fork -{a..d}-> sleep-a..sleep-d -> join(all) -> end
//	             -{choice}-> operator-choice -{continue,stop}-> join
func createDirectParallelEntryRun(t *testing.T, runID string, program engine.ProgramCommand) {
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
	nodes := map[string]model.Node{
		"fork": {Type: model.NodeTypeParallel, Next: model.Next{
			"a": "sleep-a", "b": "sleep-b", "c": "sleep-c", "d": "sleep-d", "choice": "operator-choice",
		}},
		"operator-choice": {
			Type:      model.NodeTypeDecision,
			Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Continue?"},
			Next:      model.Next{"continue": "join", "stop": "join"},
		},
		"join": join,
		"end":  {Type: model.NodeTypeEnd, Result: "success"},
	}
	for _, branch := range []string{"sleep-a", "sleep-b", "sleep-c", "sleep-d"} {
		nodes[branch] = task("join")
	}
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind,
		ID: "m2-parallel-joinall-validation", Start: "fork", Nodes: nodes,
	})
}

func createDirectTaskEntryRun(t *testing.T, runID string, program engine.ProgramCommand) {
	t.Helper()
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-direct-task-entry", Start: "task",
		Nodes: map[string]model.Node{
			"task": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram, Profile: program.Profile,
					Run: program.Run, Args: append([]string(nil), program.Args...), Timeout: program.Timeout,
				},
				Next: model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	})
}

func createDirectDecisionEntryRun(t *testing.T, runID string, program engine.ProgramCommand) {
	t.Helper()
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-direct-decision-entry", Start: "decide",
		Nodes: map[string]model.Node{
			"decide": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Proceed?"},
				Next:      model.Next{"go": "task", "stop": "canceled"},
			},
			"task": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram, Profile: program.Profile,
					Run: program.Run, Args: append([]string(nil), program.Args...), Timeout: program.Timeout,
				},
				Next: model.Next{model.DefaultOutcome: "end"},
			},
			"end":      {Type: model.NodeTypeEnd},
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	})
}

// TestDirectParallelEntryRunCompletesThroughJoinAll is the deployed-failure
// regression: this exact template shape was refused before a run could be
// created. It now runs every branch, takes the sibling verdict, and reduces at
// the join: all task.
func TestDirectParallelEntryRunCompletesThroughJoinAll(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createDirectParallelEntryRun(t, "run_direct_parallel_entry", program)

	run := mustLoadRun(t, "run_direct_parallel_entry")
	require.Equal(t, ActionContinue, run.Action().Kind)

	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch, "a fork entry must fan out without an explicit start node")

	var executed []string
	for dispatch != nil {
		executed = append(executed, dispatch.command.NodeID)
		_, next, err := executeForTest(t.Context(), run, dispatch, Authorization{RunID: run.ID(), Profile: program.Profile})
		require.NoError(t, err)
		dispatch = next
		if next == nil && len(run.AwaitingDecisions()) > 0 {
			// The decision branch is the last thing gating the join.
			require.NoError(t, RecordDecision(run, "operator", DecisionInput{
				NodeID: "operator-choice", Verdict: "continue",
			}))
			dispatch, err = Prepare(run)
			require.NoError(t, err)
		}
	}

	assert.Equal(t, []string{"sleep-a", "sleep-b", "sleep-c", "sleep-d", "join"}, executed,
		"every branch and then the reducer must run, in deterministic order")
	assert.Equal(t, ActionTerminal, run.Action().Kind)

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.RunCompleted, checkpoint.Status)
	assert.Empty(t, checkpoint.Commands)
	assert.Empty(t, checkpoint.AwaitingDecisions)
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["fork"])
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["join"])
	assert.Equal(t, engine.EdgeArrived, checkpoint.Edges["operator-choice"]["continue"])
	assert.Equal(t, engine.EdgeNotTaken, checkpoint.Edges["operator-choice"]["stop"])
}

// TestDirectTaskEntryRunDispatchesWithoutAnAdvance proves a task entry is
// dispatchable straight out of creation: there is no engine-owned node in front
// of it to advance through.
func TestDirectTaskEntryRunDispatchesWithoutAnAdvance(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createDirectTaskEntryRun(t, "run_direct_task_entry", program)

	run := mustLoadRun(t, "run_direct_task_entry")
	require.Equal(t, ActionContinue, run.Action().Kind)

	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	assert.Equal(t, "task", dispatch.command.NodeID)

	_, next, err := executeForTest(t.Context(), run, dispatch, Authorization{RunID: run.ID(), Profile: program.Profile})
	require.NoError(t, err)
	assert.Nil(t, next)
	assert.Equal(t, ActionTerminal, run.Action().Kind)
	assert.Equal(t, engine.RunCompleted, run.Action().Status)
}

// TestDirectDecisionEntryRunAwaitsAtColdLoadAndResolves is the restart case for
// a run that is parked on a human from creation: nothing ever advanced it, so
// the obligation the creation boundary wrote is the only thing a cold load has
// to go on.
func TestDirectDecisionEntryRunAwaitsAtColdLoadAndResolves(t *testing.T) {
	setupExecutorTest(t)
	program := helperProgram(t, "success")
	createDirectDecisionEntryRun(t, "run_direct_decision_entry", program)

	cold := mustLoadRun(t, "run_direct_decision_entry")
	action := cold.Action()
	require.Equal(t, ActionAwaitDecision, action.Kind, "a decision entry parks the run before anything runs")
	require.NotNil(t, action.Decision)
	assert.Equal(t, "decide", action.Decision.NodeID)
	assert.Equal(t, []string{"go", "stop"}, cold.VerdictsFor("decide"))

	// A resume must be a durable no-op: there is nothing to plan.
	versionBefore := cold.StateVersion()
	dispatch, err := Prepare(cold)
	require.NoError(t, err)
	require.Nil(t, dispatch)
	assert.Equal(t, versionBefore, cold.StateVersion())

	require.NoError(t, RecordDecision(cold, "operator", DecisionInput{
		NodeID: "decide", Verdict: "go", Evidence: "entry decision answered",
	}))
	// The obligation is one-shot even though no advance ever created it.
	assert.ErrorIs(t, RecordDecision(cold, "operator", DecisionInput{NodeID: "decide", Verdict: "stop"}),
		engine.ErrStaleDecision)

	dispatch, err = Prepare(cold)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	assert.Equal(t, "task", dispatch.command.NodeID)
	_, next, err := executeForTest(t.Context(), cold, dispatch, Authorization{RunID: cold.ID(), Profile: program.Profile})
	require.NoError(t, err)
	assert.Nil(t, next)
	assert.Equal(t, engine.RunCompleted, cold.Action().Status)
	assert.Equal(t, []string{"decision_recorded", "program_prepared", "program_observed", "engine_advanced"},
		eventKinds(t, "run_direct_decision_entry"),
		"the fixture writes no creation evidence; the daemon creation path does")
}
