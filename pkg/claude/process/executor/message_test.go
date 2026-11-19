package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func createMessageRun(t *testing.T, runID string) {
	t.Helper()
	createRunFromTemplate(t, runID, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "message-test", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "tell"}},
			"tell": {Type: model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerMessage, To: []string{"operator"}, Text: "Done."},
				Next:      model.Next{model.DefaultOutcome: "end"}},
			"end": {Type: model.NodeTypeEnd},
		},
	})
}

// installMessageSender swaps the daemon's sender for one that records what
// it was asked to send and answers outcome.
func installMessageSender(t *testing.T, outcome engine.ProgramOutcome) *[]engine.Command {
	t.Helper()
	var sent []engine.Command
	previous := messagePerform
	messagePerform = func(_ context.Context, _ string, command engine.Command) (Result, error) {
		sent = append(sent, command)
		return Result{Observation: engine.ProgramObservation{CommandID: command.ID, NodeID: command.NodeID, Outcome: outcome}}, nil
	}
	t.Cleanup(func() { messagePerform = previous })
	return &sent
}

func TestMessageCommandIsSentWithoutAProgramProfile(t *testing.T) {
	setupExecutorTest(t)
	sent := installMessageSender(t, engine.ProgramSucceeded)
	createMessageRun(t, "run_message_sent")
	run := mustLoadRun(t, "run_message_sent")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	command := dispatch.Command()
	require.Equal(t, engine.CommandMessage, command.Kind)
	require.NotNil(t, command.Message)
	assert.Equal(t, []string{"operator"}, command.Message.To)
	_, next, err := executeForTest(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.NoError(t, err)
	assert.Nil(t, next)
	require.Len(t, *sent, 1)
	assert.Equal(t, engine.RunCompleted, mustLoadRun(t, run.ID()).Action().Status)
	assert.Contains(t, eventKinds(t, run.ID()), "message_prepared")
	assert.Contains(t, eventKinds(t, run.ID()), "message_sent")
}

// A message the daemon stopped sending is not ambiguous like a program: a
// cold load resumes it rather than asking an operator to reconcile it.
func TestAnInterruptedMessageIsResumedNotReconciled(t *testing.T) {
	setupExecutorTest(t)
	sent := installMessageSender(t, engine.ProgramSucceeded)
	createMessageRun(t, "run_message_resumed")
	run := mustLoadRun(t, "run_message_resumed")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)

	cold := mustLoadRun(t, run.ID())
	assert.Equal(t, ActionDispatch, cold.Action().Kind, "not needs_reconcile")
	resumed, err := Prepare(cold)
	require.NoError(t, err)
	require.NotNil(t, resumed, "the cold owner resumes the message")
	assert.Equal(t, dispatch.Command().ID, resumed.Command().ID, "the same command, sent again")
	_, _, err = executeForTest(t.Context(), cold, resumed, Authorization{RunID: cold.ID()})
	require.NoError(t, err)
	require.Len(t, *sent, 1)
	assert.Equal(t, engine.RunCompleted, mustLoadRun(t, run.ID()).Action().Status)
	assert.Contains(t, eventKinds(t, run.ID()), "message_resumed")
}

func TestAFailedMessageFailsItsStep(t *testing.T) {
	setupExecutorTest(t)
	installMessageSender(t, engine.ProgramFailed)
	createMessageRun(t, "run_message_failed")
	run := mustLoadRun(t, "run_message_failed")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	_, _, err = executeForTest(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.NoError(t, err)
	assert.Equal(t, engine.RunFailed, mustLoadRun(t, run.ID()).Action().Status)
}

// A message this owner gave up on (its result would not commit) is not sent
// again by the same owner, so a failure that repeats cannot resend it in a
// loop; a fresh load resumes it.
func TestAnAbandonedMessageWaitsForAFreshLoad(t *testing.T) {
	setupExecutorTest(t)
	installMessageSender(t, engine.ProgramSucceeded)
	createMessageRun(t, "run_message_abandoned")
	run := mustLoadRun(t, "run_message_abandoned")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	Abandon(run, dispatch)
	again, err := Prepare(run)
	require.NoError(t, err)
	assert.Nil(t, again, "not resent by the owner that abandoned it")
	resumed, err := Prepare(mustLoadRun(t, run.ID()))
	require.NoError(t, err)
	assert.NotNil(t, resumed, "a fresh load resumes it")
}
