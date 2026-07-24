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

// createDecisionRun persists a run whose first stop is a human decision:
// start -> decide {go: task, stop: canceled-end}, task -> end.
func createDecisionRun(t *testing.T, runID string, program engine.ProgramCommand) {
	t.Helper()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-decision-test", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "decide"}},
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
	}
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

// prepareToDecision advances a fresh load until the decision is awaited and
// asserts the quiescent shape a resume observes.
func prepareToDecision(t *testing.T, runID string) *Run {
	t.Helper()
	run := mustLoadRun(t, runID)
	require.Equal(t, ActionContinue, run.Action().Kind)
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.Nil(t, dispatch, "an awaited decision yields no dispatch")
	action := run.Action()
	require.Equal(t, ActionAwaitDecision, action.Kind)
	require.NotNil(t, action.Decision)
	require.Equal(t, "decide", action.Decision.NodeID)
	require.Equal(t, []string{"go", "stop"}, run.VerdictsFor(action.Decision.NodeID))
	return run
}

func TestRecordDecisionCommitsVerdictEdgeAndEvidenceAtomically(t *testing.T) {
	setupExecutorTest(t)
	createDecisionRun(t, "run_decide", helperProgram(t, "success"))
	run := prepareToDecision(t, "run_decide")
	versionBefore := run.StateVersion()

	require.NoError(t, RecordDecision(run, "operator@example", DecisionInput{
		NodeID: "decide", Verdict: "go", Evidence: "checked the intake report",
	}))
	assert.Equal(t, versionBefore+1, run.StateVersion())
	assert.Equal(t, ActionContinue, run.Action().Kind)
	assert.Empty(t, run.AwaitingDecisions(), "the resolved obligation is consumed")

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Nil(t, checkpoint.FirstAwaitingDecision())
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["decide"])
	assert.Equal(t, engine.NodeReady, checkpoint.Nodes["task"])
	assert.Equal(t, engine.NodeSkipped, checkpoint.Nodes["canceled"])
	assert.Equal(t, engine.EdgeArrived, checkpoint.Edges["decide"]["go"])
	assert.Equal(t, engine.EdgeNotTaken, checkpoint.Edges["decide"]["stop"])

	assert.Equal(t, []string{"decision_awaited", "decision_recorded"}, eventKinds(t, run.ID()))
	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	decisionEvent := events[len(events)-1]
	assert.Equal(t, "decide", decisionEvent.NodeID)
	assert.Equal(t, "operator@example", decisionEvent.Actor)
	var payload struct {
		Verdict    string            `json:"verdict"`
		Evidence   string            `json:"evidence"`
		ChosenEdge engine.ChosenEdge `json:"chosenEdge"`
	}
	require.NoError(t, decisionEvent.DecodePayload(&payload))
	assert.Equal(t, "go", payload.Verdict)
	assert.Equal(t, "checked the intake report", payload.Evidence)
	assert.Equal(t, engine.ChosenEdge{From: "decide", Outcome: "go", To: "task"}, payload.ChosenEdge)

	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch, "the chosen branch task must become dispatchable")
	assert.Equal(t, "task", dispatch.command.NodeID)
}

func TestRecordDecisionTerminalVerdictClosesImpossibleBranch(t *testing.T) {
	setupExecutorTest(t)
	createDecisionRun(t, "run_decide_stop", helperProgram(t, "success"))
	run := prepareToDecision(t, "run_decide_stop")

	require.NoError(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "stop"}))
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	assert.Nil(t, dispatch)
	action := run.Action()
	assert.Equal(t, ActionTerminal, action.Kind)
	assert.Equal(t, engine.RunCanceled, action.Status)

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeSkipped, checkpoint.Nodes["task"])
	assert.Equal(t, engine.NodeSkipped, checkpoint.Nodes["end"])
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["canceled"])
}

func TestRecordDecisionRefusesStaleDuplicateWrongNodeAndInvalidInput(t *testing.T) {
	setupExecutorTest(t)
	createDecisionRun(t, "run_decide_refuse", helperProgram(t, "success"))
	run := prepareToDecision(t, "run_decide_refuse")

	assert.ErrorIs(t, RecordDecision(run, "", DecisionInput{NodeID: "decide", Verdict: "go"}), ErrInvalidActor)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{Verdict: "go"}), ErrInvalidDecisionInput)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide"}), ErrInvalidDecisionInput)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "go\x00"}), ErrInvalidDecisionInput)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{
		NodeID: "decide", Verdict: "go", Evidence: string(make([]byte, MaxDecisionEvidenceBytes+1)),
	}), ErrInvalidDecisionInput)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "task", Verdict: "go"}), engine.ErrStaleDecision)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "maybe"}), engine.ErrInvalidDecisionVerdict)
	assert.Equal(t, ActionAwaitDecision, run.Action().Kind, "refused input must not consume the obligation")

	require.NoError(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "go"}))
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "go"}), engine.ErrStaleDecision)
	assert.ErrorIs(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "stop"}), engine.ErrStaleDecision)
}

func TestConcurrentDecisionAttemptsLoseTheStateVersionCAS(t *testing.T) {
	setupExecutorTest(t)
	createDecisionRun(t, "run_decide_race", helperProgram(t, "success"))
	first := prepareToDecision(t, "run_decide_race")
	second := mustLoadRun(t, "run_decide_race")
	require.Equal(t, first.StateVersion(), second.StateVersion())

	require.NoError(t, RecordDecision(first, "operator-a", DecisionInput{NodeID: "decide", Verdict: "go"}))
	err := RecordDecision(second, "operator-b", DecisionInput{NodeID: "decide", Verdict: "stop"})
	assert.ErrorIs(t, err, db.ErrProcessRunVersionConflict)

	record, err := db.GetProcessRun("run_decide_race")
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.EdgeArrived, checkpoint.Edges["decide"]["go"], "the losing verdict must not overwrite the committed one")
	assert.Equal(t, []string{"decision_awaited", "decision_recorded"}, eventKinds(t, "run_decide_race"))
}

func TestRecordDecisionRollsBackCheckpointWhenEvidenceCannotCommit(t *testing.T) {
	setupExecutorTest(t)
	createDecisionRun(t, "run_decide_rollback", helperProgram(t, "success"))
	run := prepareToDecision(t, "run_decide_rollback")
	versionBefore := run.StateVersion()

	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER reject_decision_recorded
		BEFORE INSERT ON process_run_events WHEN NEW.kind = 'decision_recorded'
		BEGIN SELECT RAISE(ABORT, 'injected evidence failure'); END`)
	require.NoError(t, err)

	require.Error(t, RecordDecision(run, "operator", DecisionInput{NodeID: "decide", Verdict: "go"}))
	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	assert.Equal(t, versionBefore, record.StateVersion)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	require.NotNil(t, checkpoint.FirstAwaitingDecision(), "a failed evidence insert must roll back the decision")
	assert.Equal(t, engine.EdgeUnresolved, checkpoint.Edges["decide"]["go"])
}
