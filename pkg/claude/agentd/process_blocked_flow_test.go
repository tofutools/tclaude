package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// processBlockingTemplate is the sequential runtime fixture whose single task
// authors a retry budget, so exhausting it parks the branch.
func processBlockingTemplate(id string, maxAttempts int) *model.Template {
	return processRetryTemplate(id, maxAttempts)
}

// processBlockingForkTemplate forks into one retry-authored branch and one
// plain sibling, both reducing at a join: all. It is the shape every
// blocked-beside-live-work assertion needs.
func processBlockingForkTemplate(id string, maxAttempts int) *model.Template {
	task := func(next string) model.Node {
		return model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": next},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		}
	}
	parked := task("join")
	parked.Retry = &model.RetryPolicy{MaxAttempts: maxAttempts}
	join := task("end")
	join.Join = model.JoinAll
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{"next": "fork"}},
			"fork":   {Type: model.NodeTypeParallel, Next: model.Next{"parked": "parked", "live": "live"}},
			"parked": parked,
			"live":   task("join"),
			"join":   join,
			"end":    {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

type processResolveResponse struct {
	Started bool                  `json:"started"`
	Run     processRuntimeRunView `json:"run"`
}

func resolveBlocked(t *testing.T, f *testharness.Flow, runID string, body map[string]any) *processResolveResponse {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+runID+"/resolve-blocked", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var response processResolveResponse
	testharness.DecodeJSON(t, rec, &response)
	return &response
}

func processRunEventPage(t *testing.T, f *testharness.Flow, runID string) processRuntimeEventPage {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID+"/events?limit=16", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, rec, &page)
	return page
}

// TestProcessRuntimeExhaustedBudgetParksAndIsResolvedByRetryThenSkip is the
// end-to-end operator loop: the budget runs out, the run parks instead of
// failing, show says exactly what to resolve, a retry runs one more authored
// window, and a skip finally carries the run to completion.
func TestProcessRuntimeExhaustedBudgetParksAndIsResolvedByRetryThenSkip(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingTemplate("parking", 2))

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		return processObservation(command, engine.ProgramFailed, 1), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "parking", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, _ := log.snapshot()
	require.Len(t, dispatched, 2, "the authored budget is spent exactly once")

	// The run is parked, not failed, and show names the exact resolution
	// identity without anyone reading evidence.
	blocked := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, blocked.Status, "a parked branch must not doom the run")
	assert.Equal(t, "blocked", blocked.Action)
	require.Len(t, blocked.Blocked, 1)
	assert.Equal(t, "task-01", blocked.Blocked[0].NodeID)
	assert.Equal(t, 2, blocked.Blocked[0].Attempt)
	assert.NotEmpty(t, blocked.Blocked[0].Reason)
	assert.Empty(t, blocked.Commands, "a parked run owes no reconciliation")
	assert.False(t, blocked.NeedsReconcile)
	parkedVersion := blocked.StateVersion

	// A blocked-only run is parked work, so the periodic sweep must leave it
	// alone rather than reloading and re-preparing it every tick.
	for range 3 {
		agentd.RunProcessRunSweepForTest()
		agentd.WaitForProcessRunRuntimeForTest()
	}
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())
	stillParked := showProcessRun(t, f, run.ID)
	assert.Equal(t, parkedVersion, stillParked.StateVersion, "the sweep churned a parked run")
	redispatched, _ := log.snapshot()
	assert.Len(t, redispatched, 2, "the sweep re-ran a parked branch")

	// An explicit resume on a parked run is a no-op, not an error: there is
	// simply nothing to push until an operator resolves the branch.
	resumed := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/"+run.ID+"/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, resumed.Code, resumed.Body.String())
	assert.Contains(t, resumed.Body.String(), `"started":false`)
	assert.Equal(t, parkedVersion, showProcessRun(t, f, run.ID).StateVersion,
		"an explicit resume churned a parked run")

	// Retry opens one fresh authored-size window: attempts 3 and 4 run, both
	// with identities no earlier attempt used, and the branch parks again.
	resolved := resolveBlocked(t, f, run.ID, map[string]any{
		"nodeId": "task-01", "attempt": 2, "action": "retry", "note": "restarted the fixture host",
	})
	assert.True(t, resolved.Started)
	agentd.WaitForProcessRunRuntimeForTest()
	afterRetry, _ := log.snapshot()
	require.Len(t, afterRetry, 4, "retry did not open exactly one authored window")
	identities := map[string]struct{}{}
	for index, command := range afterRetry {
		assert.Equal(t, index+1, command.Attempt, "attempts stayed monotonic across the operator retry")
		assert.NotContains(t, identities, command.ID, "an operator retry reused a command identity")
		identities[command.ID] = struct{}{}
	}
	reblocked := showProcessRun(t, f, run.ID)
	require.Len(t, reblocked.Blocked, 1)
	assert.Equal(t, 4, reblocked.Blocked[0].Attempt, "the branch re-parked at its new exact attempt")

	// Skip settles the authored route and the run completes normally.
	skipped := resolveBlocked(t, f, run.ID, map[string]any{
		"nodeId": "task-01", "attempt": 4, "action": "skip",
	})
	// Nothing STARTED: the skip settled the last route, so the run advanced
	// straight to its end rather than planning another program.
	assert.False(t, skipped.Started)
	agentd.WaitForProcessRunRuntimeForTest()
	completed := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, completed.Status)
	assert.Empty(t, completed.Blocked)
	assert.Equal(t, engine.NodeDone, completed.Checkpoint.Nodes["task-01"])
	final, _ := log.snapshot()
	assert.Len(t, final, 4, "a skip must not run the program again")

	// The public evidence tells the whole story, bounded and attributed.
	page := processRunEventPage(t, f, run.ID)
	var blockedRows, resolvedRows int
	for _, event := range page.Events {
		switch event.Kind {
		case "node_blocked":
			blockedRows++
			assert.Equal(t, "engine:program-executor", event.Actor)
		case "blocked_resolved":
			resolvedRows++
			assert.Equal(t, "human:operator", event.Actor, "the resolving actor is the authenticated caller")
			var payload struct {
				NodeID  string `json:"nodeId"`
				Attempt int    `json:"attempt"`
				Action  string `json:"action"`
			}
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			assert.Equal(t, "task-01", payload.NodeID)
			assert.NotZero(t, payload.Attempt)
			assert.Contains(t, []string{"retry", "skip"}, payload.Action)
		}
	}
	assert.Equal(t, 2, blockedRows, "each parking is recorded once")
	assert.Equal(t, 2, resolvedRows, "each resolution is recorded once")
}

// TestProcessRuntimeBlockedBranchDoesNotStopItsSibling proves parking is
// branch-local through the whole daemon: the unaffected sibling is planned,
// executed, and observed while the other branch waits for a person.
func TestProcessRuntimeBlockedBranchDoesNotStopItsSibling(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingForkTemplate("parked-sibling", 1))

	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		if command.NodeID == "parked" {
			return processObservation(command, engine.ProgramFailed, 1), nil
		}
		return processObservation(command, engine.ProgramSucceeded, 0), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "parked-sibling", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, canceled := log.snapshot()
	nodes := map[string]int{}
	for _, command := range dispatched {
		nodes[command.NodeID]++
	}
	assert.Equal(t, 1, nodes["parked"], "the parked branch ran its single authored attempt")
	assert.Equal(t, 1, nodes["live"], "the unaffected sibling must still be planned and run")
	assert.Empty(t, canceled, "a parked branch must not tear its siblings' contexts down")

	blocked := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, blocked.Status)
	require.Len(t, blocked.Blocked, 1)
	assert.Equal(t, "parked", blocked.Blocked[0].NodeID)
	assert.Equal(t, engine.NodeDone, blocked.Checkpoint.Nodes["live"])
	// The join: all reducer still owes the parked branch, so nothing past it ran.
	assert.Equal(t, engine.NodePending, blocked.Checkpoint.Nodes["join"])

	resolveBlocked(t, f, run.ID, map[string]any{
		"nodeId": "parked", "attempt": 1, "action": "skip",
	})
	agentd.WaitForProcessRunRuntimeForTest()
	completed := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, completed.Status,
		"resolving the parked branch released the whole run")
}

// TestProcessRuntimeCancelDrainsInFlightSiblingToCanceled is the honest-drain
// property end to end: cancelling dooms the run while a sibling's program is
// genuinely still executing, that program's real observation is still accepted,
// and only then does the run finish canceled.
func TestProcessRuntimeCancelDrainsInFlightSiblingToCanceled(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingForkTemplate("cancel-drain", 1))

	parked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var siblingStopped atomic.Bool
	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		if command.NodeID == "parked" {
			once.Do(func() { close(parked) })
			return processObservation(command, engine.ProgramFailed, 1), nil
		}
		// The live sibling stays in flight until the test releases it, so the
		// cancel below really does land on a running program. It deliberately
		// ignores its context until then, so the drain assertions below are not
		// racing the best-effort stop; whether that stop reached it is recorded
		// rather than acted on.
		<-release
		siblingStopped.Store(ctx.Err() != nil)
		return processObservation(command, engine.ProgramSucceeded, 0), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "cancel-drain", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	<-parked

	// Wait for the parked branch's obligation to be durable while its sibling is
	// still executing — that is the state the cancel has to be routed into.
	var blocked processRuntimeRunView
	require.Eventually(t, func() bool {
		blocked = showProcessRun(t, f, run.ID)
		return len(blocked.Blocked) == 1
	}, 10*time.Second, 10*time.Millisecond, "the exhausted branch never parked")
	require.Len(t, blocked.Commands, 1, "the sibling's command must still be outstanding")
	assert.Equal(t, "live", blocked.Commands[0].NodeID)
	assert.Equal(t, "executing", blocked.Commands[0].State)

	// The resolution is routed to the live owner rather than refused as claimed.
	canceledRun := resolveBlocked(t, f, run.ID, map[string]any{
		"nodeId": "parked", "attempt": 1, "action": "cancel", "note": "giving up on this run",
	})
	assert.True(t, canceledRun.Started, "the run was already being driven")
	draining := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, draining.Status, "a run with a live command drains before it ends")
	assert.Equal(t, engine.NodeCanceled, draining.Checkpoint.Nodes["parked"])
	assert.Empty(t, draining.Blocked, "cancel must leave no resolution on offer")
	require.Len(t, draining.Commands, 1, "the sibling's durable command survives the cancel")

	close(release)
	agentd.WaitForProcessRunRuntimeForTest()
	assert.True(t, siblingStopped.Load(),
		"cancel must stop sibling programs best-effort, as a failure does")
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCanceled, final.Status)
	assert.Empty(t, final.Checkpoint.Commands, "the canceled run drained its outbox")
	assert.Equal(t, "terminal", final.Action)
	// The sibling's real observation was accepted, not discarded.
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["live"])

	// A doomed run offers no further resolution.
	late := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/"+run.ID+"/resolve-blocked", map[string]any{
			"nodeId": "parked", "attempt": 1, "action": "retry",
		})
	assert.Equal(t, http.StatusConflict, late.Code, late.Body.String())
	assert.Contains(t, late.Body.String(), "process_blocked_stale")
}

// TestProcessRuntimeBlockedResolutionRefusesStaleDuplicateAndInvalidInput is
// the wire-level fail-closed table, including the shapes only the HTTP surface
// can produce.
func TestProcessRuntimeBlockedResolutionRefusesStaleDuplicateAndInvalidInput(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingTemplate("refusing", 1))
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(_ context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		return processObservation(dispatch.Command(), engine.ProgramFailed, 1), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "refusing", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	agentd.WaitForProcessRunRuntimeForTest()
	require.Len(t, showProcessRun(t, f, run.ID).Blocked, 1)

	for _, test := range []struct {
		name string
		body map[string]any
		code int
		text string
	}{
		{name: "wrong attempt", code: http.StatusConflict, text: "process_blocked_stale",
			body: map[string]any{"nodeId": "task-01", "attempt": 2, "action": "retry"}},
		{name: "wrong node", code: http.StatusConflict, text: "process_blocked_stale",
			body: map[string]any{"nodeId": "end", "attempt": 1, "action": "retry"}},
		{name: "unknown node", code: http.StatusConflict, text: "process_blocked_stale",
			body: map[string]any{"nodeId": "nope", "attempt": 1, "action": "retry"}},
		{name: "unknown action", code: http.StatusUnprocessableEntity, text: "process_blocked_invalid",
			body: map[string]any{"nodeId": "task-01", "attempt": 1, "action": "reroute"}},
		{name: "missing attempt", code: http.StatusUnprocessableEntity, text: "process_blocked_invalid",
			body: map[string]any{"nodeId": "task-01", "action": "retry"}},
		{name: "missing node", code: http.StatusUnprocessableEntity, text: "process_blocked_invalid",
			body: map[string]any{"attempt": 1, "action": "retry"}},
		{name: "unknown request field", code: http.StatusBadRequest, text: "process_run_request",
			body: map[string]any{"nodeId": "task-01", "attempt": 1, "action": "retry", "owner": "someone"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := processRuntimeRequest(t, f, http.MethodPost,
				"/v1/process/runs/"+run.ID+"/resolve-blocked", test.body)
			assert.Equal(t, test.code, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), test.text)
			assert.Len(t, showProcessRun(t, f, run.ID).Blocked, 1,
				"refused input must not consume the obligation")
		})
	}

	// The one valid resolution commits, and replaying it fails closed.
	resolveBlocked(t, f, run.ID, map[string]any{"nodeId": "task-01", "attempt": 1, "action": "skip"})
	duplicate := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/"+run.ID+"/resolve-blocked",
		map[string]any{"nodeId": "task-01", "attempt": 1, "action": "skip"})
	assert.Equal(t, http.StatusConflict, duplicate.Code, duplicate.Body.String())
	assert.Contains(t, duplicate.Body.String(), "process_blocked_stale")
}

// TestProcessRuntimeBlockedRunSurvivesRestartAndResolvesAfterColdLoad models a
// daemon restart: only SQLite survives, the production startup sweep runs, and
// the parked branch is still exactly resolvable afterwards.
func TestProcessRuntimeBlockedRunSurvivesRestartAndResolvesAfterColdLoad(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processBlockingTemplate("restarting", 1))
	log := &processAttemptLog{}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		log.record(ctx, command)
		return processObservation(command, engine.ProgramFailed, 1), nil
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "restarting", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	agentd.WaitForProcessRunRuntimeForTest()
	parked := showProcessRun(t, f, run.ID)
	require.Len(t, parked.Blocked, 1)

	// Discard every in-memory handle and run the production startup page twice.
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()

	dispatched, _ := log.snapshot()
	assert.Len(t, dispatched, 1, "a cold-loaded parked run must not re-run its exhausted attempt")
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())

	reloaded := showProcessRun(t, f, run.ID)
	assert.Equal(t, parked.StateVersion, reloaded.StateVersion, "the restart churned a parked run")
	require.Len(t, reloaded.Blocked, 1)
	assert.Equal(t, "task-01", reloaded.Blocked[0].NodeID)
	assert.Equal(t, 1, reloaded.Blocked[0].Attempt)

	resolveBlocked(t, f, run.ID, map[string]any{"nodeId": "task-01", "attempt": 1, "action": "skip"})
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, engine.RunCompleted, showProcessRun(t, f, run.ID).Status)
}
