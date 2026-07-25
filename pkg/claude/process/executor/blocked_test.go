package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// createBlockingRun persists start -> task -> end where the task authors a
// retry budget, so exhausting it parks the branch instead of failing the run.
func createBlockingRun(t *testing.T, runID string, maxAttempts int, program engine.ProgramCommand) {
	t.Helper()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-blocked-test", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "task"}},
			"task": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram, Profile: program.Profile,
					Run: program.Run, Args: append([]string(nil), program.Args...), Timeout: program.Timeout,
				},
				Retry: &model.RetryPolicy{MaxAttempts: maxAttempts},
				Next:  model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	createRunFromTemplate(t, runID, tmpl)
}

// blockOneBranch drives a run until its single task has exhausted its budget
// and parked. Each observation plans the next attempt itself, exactly as the
// daemon owner would, so the loop follows the dispatch chain rather than asking
// for a fresh plan it has not accounted for.
func blockOneBranch(t *testing.T, runID string, wantAttempts int) *Run {
	t.Helper()
	run := mustLoadRun(t, runID)
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	attempts := 0
	for dispatch != nil {
		attempts++
		require.LessOrEqual(t, attempts, wantAttempts, "the branch ran past its authored budget")
		_, next, err := executeForTest(t.Context(), run, dispatch,
			Authorization{RunID: run.ID(), Profile: dispatch.Command().Program.Profile})
		require.NoError(t, err)
		dispatch = next
	}
	require.Equal(t, wantAttempts, attempts, "the branch did not spend its whole budget")
	require.Len(t, run.Blocked(), 1, "the exhausted branch did not park")
	return run
}

// TestExhaustedBranchParksWithEvidenceInTheSameCommit is the evidence
// contract: the node_blocked row and the durable obligation are created by one
// transaction, so neither can exist without the other, and the row carries the
// exact attempt plus the parking time the checkpoint deliberately omits.
func TestExhaustedBranchParksWithEvidenceInTheSameCommit(t *testing.T) {
	setupExecutorTest(t)
	createBlockingRun(t, "run_blocked", 2, helperProgram(t, "unrecognized-mode"))
	run := blockOneBranch(t, "run_blocked", 2)

	assert.Equal(t, "task", run.Blocked()[0].NodeID)
	assert.Equal(t, 2, run.BlockedAttempt("task"), "the blocked attempt is derived from the counter")
	assert.Zero(t, run.BlockedAttempt("end"), "only a blocked node reports a blocked attempt")
	assert.False(t, run.Draining(), "a parked branch is not a doomed run")
	assert.Equal(t, ActionBlocked, run.Action().Kind,
		"a parked run has an honest coarse summary, not an error")

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	assert.Equal(t, string(engine.RunRunning), record.Status)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeBlocked, checkpoint.Nodes["task"])
	require.Len(t, checkpoint.Blocked, 1)
	assert.Empty(t, checkpoint.Commands, "the exhausted attempt's command is consumed")

	// Exactly one node_blocked row, in the commit that observed the last attempt.
	kinds := eventKinds(t, run.ID())
	assert.Equal(t, 1, countKind(kinds, "node_blocked"), "kinds = %v", kinds)
	blockedEvent := eventOfKind(t, run.ID(), "node_blocked")
	// The parking row belongs to the same commit as the observation that caused
	// it, so it follows that observation immediately in the public stream.
	assert.Equal(t, []string{"program_observed", "node_blocked"},
		kinds[len(kinds)-3:len(kinds)-1], "kinds = %v", kinds)
	assert.Equal(t, "task", blockedEvent.NodeID)
	assert.Equal(t, EngineActor, blockedEvent.Actor, "nobody asked for this; exhaustion produced it")
	assert.False(t, blockedEvent.OccurredAt.IsZero(), "the parking time lives in evidence, not the checkpoint")
	var payload struct {
		NodeID  string `json:"nodeId"`
		Attempt int    `json:"attempt"`
		Reason  string `json:"reason"`
	}
	require.NoError(t, blockedEvent.DecodePayload(&payload))
	assert.Equal(t, "task", payload.NodeID)
	assert.Equal(t, 2, payload.Attempt)
	assert.NotEmpty(t, payload.Reason)
	// The public evidence stays bounded even though the reason is copied text.
	assert.LessOrEqual(t, len(payload.Reason), engine.MaxBlockedReasonBytes)
}

// TestResolveBlockedRetryReadiesTheBranchAndRecordsTheActor covers the audited
// happy path: the resolution commits with the authenticated caller as actor,
// and the branch is dispatchable again at the next attempt.
func TestResolveBlockedRetryReadiesTheBranchAndRecordsTheActor(t *testing.T) {
	setupExecutorTest(t)
	createBlockingRun(t, "run_blocked_retry", 1, helperProgram(t, "unrecognized-mode"))
	run := blockOneBranch(t, "run_blocked_retry", 1)
	versionBefore := run.StateVersion()

	require.NoError(t, ResolveBlocked(run, "operator@example", ResolutionInput{
		NodeID: "task", Attempt: 1, Action: engine.ResolveRetry, Note: "infra was down",
	}))
	assert.Equal(t, versionBefore+1, run.StateVersion())
	assert.Empty(t, run.Blocked(), "the resolved obligation is consumed")

	resolvedEvent := eventOfKind(t, run.ID(), "blocked_resolved")
	assert.Equal(t, "task", resolvedEvent.NodeID)
	assert.Equal(t, "operator@example", resolvedEvent.Actor, "the actor is the authenticated caller")
	var payload struct {
		NodeID  string `json:"nodeId"`
		Attempt int    `json:"attempt"`
		Action  string `json:"action"`
		Note    string `json:"note"`
	}
	require.NoError(t, resolvedEvent.DecodePayload(&payload))
	assert.Equal(t, "task", payload.NodeID)
	assert.Equal(t, 1, payload.Attempt)
	assert.Equal(t, string(engine.ResolveRetry), payload.Action)
	assert.Equal(t, "infra was down", payload.Note)

	// Ordinary planning mints the next attempt; the resolution dispatched nothing.
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	assert.Equal(t, 2, dispatch.Command().Attempt, "the retry window minted attempt 2, not a reused 1")
}

// TestResolveBlockedRefusesInvalidStaleAndDuplicateInput is the fail-closed
// table at the executor boundary: bad actors, bad payloads, the wrong attempt,
// and a replayed resolution are all refused without consuming the obligation.
func TestResolveBlockedRefusesInvalidStaleAndDuplicateInput(t *testing.T) {
	setupExecutorTest(t)
	createBlockingRun(t, "run_blocked_refuse", 1, helperProgram(t, "unrecognized-mode"))
	run := blockOneBranch(t, "run_blocked_refuse", 1)
	valid := ResolutionInput{NodeID: "task", Attempt: 1, Action: engine.ResolveRetry}

	assert.ErrorIs(t, ResolveBlocked(run, "", valid), ErrInvalidActor)
	for name, input := range map[string]ResolutionInput{
		"missing node":       {Attempt: 1, Action: engine.ResolveRetry},
		"control character":  {NodeID: "task\x00", Attempt: 1, Action: engine.ResolveRetry},
		"attempt zero":       {NodeID: "task", Action: engine.ResolveRetry},
		"negative attempt":   {NodeID: "task", Attempt: -1, Action: engine.ResolveRetry},
		"unknown action":     {NodeID: "task", Attempt: 1, Action: "reroute"},
		"missing action":     {NodeID: "task", Attempt: 1},
		"oversized note":     {NodeID: "task", Attempt: 1, Action: engine.ResolveRetry, Note: strings.Repeat("x", MaxResolutionNoteBytes+1)},
		"control-char note":  {NodeID: "task", Attempt: 1, Action: engine.ResolveRetry, Note: "bad\x00note"},
		"oversized node id":  {NodeID: strings.Repeat("n", MaxDecisionNodeIDBytes+1), Attempt: 1, Action: engine.ResolveRetry},
		"invalid utf-8 note": {NodeID: "task", Attempt: 1, Action: engine.ResolveRetry, Note: "\xff"},
	} {
		assert.ErrorIsf(t, ResolveBlocked(run, "operator", input), ErrInvalidResolutionInput, "case %q", name)
	}
	assert.ErrorIs(t, ResolveBlocked(run, "operator", ResolutionInput{
		NodeID: "task", Attempt: 2, Action: engine.ResolveRetry}), engine.ErrStaleResolution)
	assert.ErrorIs(t, ResolveBlocked(run, "operator", ResolutionInput{
		NodeID: "end", Attempt: 1, Action: engine.ResolveRetry}), engine.ErrStaleResolution)
	require.Len(t, run.Blocked(), 1, "refused input must not consume the obligation")

	require.NoError(t, ResolveBlocked(run, "operator", valid))
	assert.ErrorIs(t, ResolveBlocked(run, "operator", valid), engine.ErrStaleResolution,
		"a replayed resolution must fail closed")
}

// TestConcurrentBlockedResolutionsLoseTheStateVersionCAS proves the durable CAS
// serializes two operators racing on the same parked branch.
func TestConcurrentBlockedResolutionsLoseTheStateVersionCAS(t *testing.T) {
	setupExecutorTest(t)
	createBlockingRun(t, "run_blocked_race", 1, helperProgram(t, "unrecognized-mode"))
	first := blockOneBranch(t, "run_blocked_race", 1)
	second := mustLoadRun(t, "run_blocked_race")
	require.Equal(t, first.StateVersion(), second.StateVersion())

	require.NoError(t, ResolveBlocked(first, "operator-a", ResolutionInput{
		NodeID: "task", Attempt: 1, Action: engine.ResolveRetry}))
	err := ResolveBlocked(second, "operator-b", ResolutionInput{
		NodeID: "task", Attempt: 1, Action: engine.ResolveSkip})
	assert.ErrorIs(t, err, db.ErrProcessRunVersionConflict)

	record, err := db.GetProcessRun("run_blocked_race")
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeReady, checkpoint.Nodes["task"],
		"the losing resolution must not overwrite the committed one")
	assert.Equal(t, 1, countKind(eventKinds(t, "run_blocked_race"), "blocked_resolved"))
}

// TestResolveBlockedRollsBackWhenEvidenceCannotCommit keeps checkpoint and
// evidence atomic in the failure direction too.
func TestResolveBlockedRollsBackWhenEvidenceCannotCommit(t *testing.T) {
	setupExecutorTest(t)
	createBlockingRun(t, "run_blocked_rollback", 1, helperProgram(t, "unrecognized-mode"))
	run := blockOneBranch(t, "run_blocked_rollback", 1)
	versionBefore := run.StateVersion()

	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER reject_blocked_resolved
		BEFORE INSERT ON process_run_events WHEN NEW.kind = 'blocked_resolved'
		BEGIN SELECT RAISE(ABORT, 'injected evidence failure'); END`)
	require.NoError(t, err)

	require.Error(t, ResolveBlocked(run, "operator", ResolutionInput{
		NodeID: "task", Attempt: 1, Action: engine.ResolveSkip}))
	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	assert.Equal(t, versionBefore, record.StateVersion)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	require.Len(t, checkpoint.Blocked, 1, "a failed evidence insert must roll back the resolution")
	assert.Equal(t, engine.NodeBlocked, checkpoint.Nodes["task"])
}

// TestBlockedRunColdLoadsAndResolvesAfterRestart is the restart path: a fresh
// LoadRun of a parked run reconstructs the obligation and its exact attempt
// without reading evidence, and the resolution commits normally afterwards.
func TestBlockedRunColdLoadsAndResolvesAfterRestart(t *testing.T) {
	setupExecutorTest(t)
	createBlockingRun(t, "run_blocked_restart", 1, helperProgram(t, "unrecognized-mode"))
	blockOneBranch(t, "run_blocked_restart", 1)

	reloaded := mustLoadRun(t, "run_blocked_restart")
	require.Len(t, reloaded.Blocked(), 1)
	assert.Equal(t, "task", reloaded.Blocked()[0].NodeID)
	assert.Equal(t, 1, reloaded.BlockedAttempt("task"))
	// A parked run has nothing outstanding, so it is not reconcilable work.
	assert.Empty(t, reloaded.Commands())
	dispatch, err := Prepare(reloaded)
	require.NoError(t, err)
	assert.Nil(t, dispatch, "a cold-loaded blocked run must not plan anything by itself")

	require.NoError(t, ResolveBlocked(reloaded, "operator", ResolutionInput{
		NodeID: "task", Attempt: 1, Action: engine.ResolveSkip}))
	record, err := db.GetProcessRun("run_blocked_restart")
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeDone, checkpoint.Nodes["task"])
	assert.Empty(t, checkpoint.Blocked)
}

func countKind(kinds []string, kind string) int {
	count := 0
	for _, value := range kinds {
		if value == kind {
			count++
		}
	}
	return count
}

// eventOfKind returns the single public evidence row of one kind, failing if
// the run recorded any other number of them.
func eventOfKind(t *testing.T, runID, kind string) db.ProcessRunEvent {
	t.Helper()
	events, err := db.ListProcessRunEvents(runID, 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	var found []db.ProcessRunEvent
	for _, event := range events {
		if event.Kind == kind {
			found = append(found, event)
		}
	}
	require.Lenf(t, found, 1, "want exactly one %q row", kind)
	return found[0]
}
