package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// processRetryTemplate is the sequential runtime fixture with an authored retry
// budget on its single program task.
func processRetryTemplate(id string, maxAttempts int) *model.Template {
	tmpl := processRuntimeTemplate(id, 1)
	task := tmpl.Nodes["task-01"]
	task.Retry = &model.RetryPolicy{MaxAttempts: maxAttempts}
	tmpl.Nodes["task-01"] = task
	return tmpl
}

// processAttemptLog records what the daemon actually handed each worker, so the
// assertions are about observed dispatches rather than about inferred state.
type processAttemptLog struct {
	mu       sync.Mutex
	attempts []engine.Command
	canceled []string
}

func (l *processAttemptLog) record(ctx context.Context, command engine.Command) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts = append(l.attempts, command)
	if ctx.Err() != nil {
		l.canceled = append(l.canceled, command.ID)
	}
}

func (l *processAttemptLog) snapshot() ([]engine.Command, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]engine.Command(nil), l.attempts...), append([]string(nil), l.canceled...)
}

func processObservation(command engine.Command, outcome engine.ProgramOutcome, exitCode int) executor.Result {
	observation := engine.ProgramObservation{
		CommandID: command.ID, NodeID: command.NodeID, Outcome: outcome, ExitCode: exitCode,
	}
	if outcome == engine.ProgramFailed {
		observation.Error = "attempt failed"
	}
	return executor.Result{Observation: observation, Dispatched: true}
}

// preparedAttempts reads the attempt of every command the run publicly recorded
// as prepared. It is the evidence-side proof that a reader can tell attempts
// apart from the existing command payload alone, with no new history model.
func preparedAttempts(t *testing.T, page processRuntimeEventPage) []int {
	t.Helper()
	var attempts []int
	for _, event := range page.Events {
		if event.Kind != "program_prepared" {
			continue
		}
		var payload struct {
			Command engine.Command `json:"command"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		attempts = append(attempts, payload.Command.Attempt)
	}
	return attempts
}

// TestProcessRuntimeRetriesFailedProgramUntilItSucceeds is the end-to-end
// happy path: two failed attempts inside the authored budget are retried, the
// third succeeds, and the run completes normally.
func TestProcessRuntimeRetriesFailedProgramUntilItSucceeds(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRetryTemplate("retrying", 3))

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		if command.Attempt < 3 {
			return processObservation(command, engine.ProgramFailed, 1), nil
		}
		return processObservation(command, engine.ProgramSucceeded, 0), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "retrying", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createdRun processRuntimeRunView
	testharness.DecodeJSON(t, created, &createdRun)
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, canceled := log.snapshot()
	require.Len(t, dispatched, 3, "each attempt is dispatched exactly once")
	ids := map[string]struct{}{}
	for index, command := range dispatched {
		assert.Equal(t, index+1, command.Attempt, "attempts are dispatched in order from 1")
		assert.NotContains(t, ids, command.ID, "two attempts shared a command identity")
		ids[command.ID] = struct{}{}
	}
	// A failed attempt inside the budget must not tear down the run's workers:
	// the retry it plans would otherwise start against a dead context.
	assert.Empty(t, canceled, "a retried attempt ran under a cancelled context")

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+createdRun.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	assert.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	assert.Equal(t, map[string]int{"task-01": 3}, shown.Checkpoint.Attempts)
	assert.Empty(t, shown.Checkpoint.Commands)
	assert.Equal(t, int64(5), shown.StateVersion,
		"creation, the first prepare, and one atomic observation/advance transaction per attempt — "+
			"a retry needs no extra commit of its own")

	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+createdRun.ID+"/events", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	kinds := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		kinds = append(kinds, event.Kind)
	}
	assert.Equal(t, []string{
		"run_created",
		"program_prepared", "program_observed",
		"program_prepared", "program_observed",
		"program_prepared", "program_observed",
		"engine_advanced",
	}, kinds, "a failed attempt and the next attempt commit in one transaction each")
	assert.Equal(t, []int{1, 2, 3}, preparedAttempts(t, page),
		"public evidence distinguishes attempts through the existing command payload")
}

// TestProcessRuntimeRefusesAnUnthrottledRetryBudget proves the cap is enforced
// where it matters: run creation. Retries here are immediate, so an unbounded
// budget would be an unthrottled spawn loop with no run-cancel verb to stop it.
// The refusal lands before a run exists at all.
func TestProcessRuntimeRefusesAnUnthrottledRetryBudget(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRetryTemplate("unthrottled", 1_000_000))

	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "unthrottled", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusUnprocessableEntity, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), `"code":"process_run_invalid"`)
	assert.Contains(t, refused.Body.String(), "nodes.task-01.retry.maxAttempts")

	list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var listed struct {
		Runs []processRuntimeRunView `json:"runs"`
	}
	testharness.DecodeJSON(t, list, &listed)
	assert.Empty(t, listed.Runs, "an over-budget template must not create a run")
}

// TestProcessRuntimeExhaustedRetryBudgetParksTheBranch pins the exhaustion
// disposition of an explicitly retry-authored task: the budget is spent
// exactly once and the branch then parks on an operator, leaving the run live
// rather than failed. The counter is still never reset by exhaustion.
func TestProcessRuntimeExhaustedRetryBudgetParksTheBranch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRetryTemplate("exhausting", 2))

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		return processObservation(command, engine.ProgramFailed, 4), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "exhausting", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createdRun processRuntimeRunView
	testharness.DecodeJSON(t, created, &createdRun)
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, _ := log.snapshot()
	require.Len(t, dispatched, 2, "the budget includes the first attempt")

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+createdRun.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	assert.Equal(t, engine.RunRunning, shown.Checkpoint.Status)
	assert.Equal(t, engine.NodeBlocked, shown.Checkpoint.Nodes["task-01"])
	assert.Equal(t, map[string]int{"task-01": 2}, shown.Checkpoint.Attempts,
		"the counter is never reset, even by exhaustion")
	assert.Empty(t, shown.Checkpoint.Commands, "the exhausted attempt's command is consumed")
	assert.Equal(t, "blocked", shown.Action)
	require.Len(t, shown.Blocked, 1)
	assert.Equal(t, 2, shown.Blocked[0].Attempt, "the parked branch names its exact attempt")
}

// TestProcessRuntimeRetryingBranchDoesNotCancelOrStallItsSibling proves retry
// stays branch-local under fan-out: a branch burning an attempt neither
// cancels the sibling program still running beside it nor renumbers it.
func TestProcessRuntimeRetryingBranchDoesNotCancelOrStallItsSibling(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processFanOutTemplate("branch-retry", 2)
	flaky := tmpl.Nodes["task-01"]
	flaky.Retry = &model.RetryPolicy{MaxAttempts: 2}
	tmpl.Nodes["task-01"] = flaky
	putProcessRuntimeTemplate(t, root, tmpl)

	log := &processAttemptLog{}
	// The sibling is held at the program boundary until the retry has actually
	// been dispatched, so "the retry ran while the sibling was still executing"
	// is observed rather than timed.
	retried := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		switch {
		case command.NodeID == "task-01" && command.Attempt == 1:
			return processObservation(command, engine.ProgramFailed, 1), nil
		case command.NodeID == "task-01":
			close(retried)
			return processObservation(command, engine.ProgramSucceeded, 0), nil
		default:
			select {
			case <-retried:
			case <-ctx.Done():
				return processObservation(command, engine.ProgramFailed, -1), nil
			}
			<-release
			if ctx.Err() != nil {
				return processObservation(command, engine.ProgramFailed, -1), nil
			}
			return processObservation(command, engine.ProgramSucceeded, 0), nil
		}
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "branch-retry", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createdRun processRuntimeRunView
	testharness.DecodeJSON(t, created, &createdRun)

	<-retried
	// The sibling's durable command is untouched by the branch that retried.
	mid := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+createdRun.ID, nil)
	require.Equal(t, http.StatusOK, mid.Code, mid.Body.String())
	var midView processRuntimeRunView
	testharness.DecodeJSON(t, mid, &midView)
	assert.Equal(t, engine.NodeRunning, midView.Checkpoint.Nodes["task-02"])
	assert.Equal(t, 1, midView.Checkpoint.Attempts["task-02"], "the sibling was renumbered")
	assert.Equal(t, 2, midView.Checkpoint.Attempts["task-01"])

	releaseOnce.Do(func() { close(release) })
	agentd.WaitForProcessRunRuntimeForTest()

	_, canceled := log.snapshot()
	assert.Empty(t, canceled, "a sibling's retry cancelled a live branch")

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+createdRun.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	assert.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	assert.Equal(t, map[string]int{"task-01": 2, "task-02": 1}, shown.Checkpoint.Attempts)
}

// TestProcessRuntimeColdLoadedRetryAttemptIsReconcilableAndReissuesInPlace
// covers restart: a run whose second attempt was outstanding when the daemon
// died cold-loads with that exact attempt, is reported reconcilable rather
// than silently re-run, and a reissue reuses the already-durable command
// instead of minting a third attempt.
func TestProcessRuntimeColdLoadedRetryAttemptIsReconcilableAndReissuesInPlace(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processRetryTemplate("cold-retry", 3)
	record := putProcessRuntimeTemplate(t, root, tmpl)

	// Build exactly the state a crash mid-retry leaves behind: attempt 1 failed,
	// attempt 2 is durable and outstanding.
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_cold_retry", definition)
	require.NoError(t, err)
	checkpoint, first, _, err := engine.AdvanceAndPlan(checkpoint, definition)
	require.NoError(t, err)
	require.NotNil(t, first)
	checkpoint, err = engine.Apply(checkpoint, definition, engine.Transition{
		Kind: engine.TransitionProgramObserved,
		Observation: &engine.ProgramObservation{
			CommandID: first.ID, NodeID: first.NodeID, Outcome: engine.ProgramFailed, ExitCode: 1,
		},
	})
	require.NoError(t, err)
	checkpoint, second, _, err := engine.AdvanceAndPlan(checkpoint, definition)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, 2, second.Attempt)
	createProcessRunFixtureWithCheckpoint(t, "run_cold_retry", record.Ref, tmpl, checkpoint)

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_cold_retry", nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	assert.True(t, shown.NeedsReconcile)
	assert.Equal(t, "needs_reconcile", shown.Action)
	require.Len(t, shown.Commands, 1)
	assert.Equal(t, second.ID, shown.Commands[0].CommandID, "the cold load renamed the outstanding attempt")
	assert.Equal(t, 2, shown.Checkpoint.Attempts["task-01"])

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		return processObservation(command, engine.ProgramSucceeded, 0), nil
	}))

	reissued := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/run_cold_retry/reissue", map[string]any{"nodeId": "task-01"})
	require.Equal(t, http.StatusAccepted, reissued.Code, reissued.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, _ := log.snapshot()
	require.Len(t, dispatched, 1)
	assert.Equal(t, second.ID, dispatched[0].ID, "reissue minted a new attempt instead of reusing the durable one")
	assert.Equal(t, 2, dispatched[0].Attempt)

	final := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_cold_retry", nil)
	require.Equal(t, http.StatusOK, final.Code, final.Body.String())
	var finalView processRuntimeRunView
	testharness.DecodeJSON(t, final, &finalView)
	assert.Equal(t, engine.RunCompleted, finalView.Checkpoint.Status)
	assert.Equal(t, map[string]int{"task-01": 2}, finalView.Checkpoint.Attempts,
		"a reissue is the same attempt, not the next one")
}
