package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessRuntimeCreateRunListShowAndAutomaticSequentialCompletion(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("sequential", 2))

	completed := make(chan struct{}, 2)
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		result, next, err := executor.Execute(ctx, run, dispatch, authorization)
		completed <- struct{}{}
		return result, next, err
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "sequential", "params": map[string]string{"branch": "main"},
		"authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createdRun processRuntimeRunView
	testharness.DecodeJSON(t, created, &createdRun)
	require.NotEmpty(t, createdRun.ID)

	<-completed
	<-completed
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())

	list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var listed struct {
		Runs []processRuntimeRunView `json:"runs"`
	}
	testharness.DecodeJSON(t, list, &listed)
	require.Len(t, listed.Runs, 1)
	assert.Equal(t, engine.RunCompleted, listed.Runs[0].Status)
	assert.NotContains(t, list.Body.String(), `"checkpoint"`)
	assert.NotContains(t, list.Body.String(), `"params"`)
	assert.NotContains(t, list.Body.String(), `"programAuthorizations"`)

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+createdRun.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	assert.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	assert.Equal(t, int64(4), shown.StateVersion,
		"creation, first prepare, and one atomic observation/advance transaction per task")
	assert.Nil(t, shown.Checkpoint.OutstandingCommand)
	assert.Equal(t, "terminal", shown.Action)
	assert.Equal(t, map[string]string{"branch": "main"}, shown.Params)
	assert.Equal(t, []string{"safe"}, shown.ProgramAuthorizations)

	firstPage := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/"+createdRun.ID+"/events?limit=2", nil)
	require.Equal(t, http.StatusOK, firstPage.Code, firstPage.Body.String())
	var paged processRuntimeEventPage
	testharness.DecodeJSON(t, firstPage, &paged)
	require.Len(t, paged.Events, 2)
	assert.Equal(t, []int64{1, 2}, []int64{paged.Events[0].Sequence, paged.Events[1].Sequence})
	assert.Equal(t, int64(2), paged.Next)

	secondPage := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/"+createdRun.ID+"/events?after=2&limit=2", nil)
	require.Equal(t, http.StatusOK, secondPage.Code, secondPage.Body.String())
	testharness.DecodeJSON(t, secondPage, &paged)
	require.Len(t, paged.Events, 2)
	assert.Equal(t, []int64{3, 4}, []int64{paged.Events[0].Sequence, paged.Events[1].Sequence})
	assert.Equal(t, int64(4), paged.Next)

	evidence := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/"+createdRun.ID+"/events", nil)
	require.Equal(t, http.StatusOK, evidence.Code, evidence.Body.String())
	var eventPage processRuntimeEventPage
	testharness.DecodeJSON(t, evidence, &eventPage)
	kinds := make([]string, 0, len(eventPage.Events))
	for _, event := range eventPage.Events {
		kinds = append(kinds, event.Kind)
		assert.NotEmpty(t, event.Payload)
	}
	assert.Equal(t, []string{"run_created", "program_prepared", "program_observed", "program_prepared", "program_observed", "engine_advanced"}, kinds)
	assert.Zero(t, eventPage.Next)
	assert.NotEmpty(t, eventPage.Events[0].Actor)
	assert.Equal(t, "task-01", eventPage.Events[1].NodeID)
}

func TestProcessRuntimeEventsEmptyNotFoundAndInvalidInputs(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("empty-events", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	createRunnableProcessRunFixture(t, "run_empty_events", record.Ref, tmpl)

	empty := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/run_empty_events/events", nil)
	require.Equal(t, http.StatusOK, empty.Code, empty.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, empty, &page)
	assert.Empty(t, page.Events)
	assert.Zero(t, page.Next)

	missing := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/run_missing/events", nil)
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
	assert.Contains(t, missing.Body.String(), `"code":"process_run_not_found"`)

	invalidRun := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/INVALID!/events", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, invalidRun.Code, invalidRun.Body.String())
	assert.Contains(t, invalidRun.Body.String(), `"code":"process_run_invalid"`)

	for _, path := range []string{
		"/v1/process/runs/run_empty_events/events?after=-1",
		"/v1/process/runs/run_empty_events/events?after=nope",
		"/v1/process/runs/run_empty_events/events?limit=0",
		"/v1/process/runs/run_empty_events/events?limit=17",
	} {
		refused := processRuntimeRequest(t, f, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusBadRequest, refused.Code, refused.Body.String())
	}
}

func TestProcessRuntimeEventsPublicMaximumAndExactFinalPage(t *testing.T) {
	const publicMaximum = 16
	f, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("maximum-events", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_maximum_events", definition)
	require.NoError(t, err)
	payload := json.RawMessage(`{"x":"` + strings.Repeat("x", db.MaxProcessRunEventPayloadBytes-8) + `"}`)
	require.Len(t, payload, db.MaxProcessRunEventPayloadBytes)
	events := make([]db.ProcessRunEvent, publicMaximum+1)
	for i := range events {
		events[i] = db.ProcessRunEvent{
			Sequence: int64(i + 1), OccurredAt: time.Date(2026, 7, 23, 12, i, 0, 0, time.UTC),
			Kind: "bounded", PayloadJSON: payload,
		}
	}
	createProcessRunFixtureWithEvents(t, "run_maximum_events", record.Ref, tmpl, checkpoint, events)

	first := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/run_maximum_events/events?limit=16", nil)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, first, &page)
	require.Len(t, page.Events, publicMaximum)
	assert.Equal(t, int64(1), page.Events[0].Sequence)
	assert.Equal(t, int64(publicMaximum), page.Events[publicMaximum-1].Sequence)
	assert.Equal(t, int64(publicMaximum), page.Next)
	for _, event := range page.Events {
		assert.Len(t, event.Payload, db.MaxProcessRunEventPayloadBytes)
	}

	exactFinal := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/run_maximum_events/events?after=1&limit=16", nil)
	require.Equal(t, http.StatusOK, exactFinal.Code, exactFinal.Body.String())
	testharness.DecodeJSON(t, exactFinal, &page)
	require.Len(t, page.Events, publicMaximum)
	assert.Equal(t, int64(2), page.Events[0].Sequence)
	assert.Equal(t, int64(publicMaximum+1), page.Events[publicMaximum-1].Sequence)
	assert.Zero(t, page.Next, "an exact-full final page must not advertise an empty continuation")

	highCursor := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/run_maximum_events/events?after=999&limit=16", nil)
	require.Equal(t, http.StatusOK, highCursor.Code, highCursor.Body.String())
	testharness.DecodeJSON(t, highCursor, &page)
	assert.Empty(t, page.Events)
	assert.Zero(t, page.Next)

	overMaximum := processRuntimeRequest(t, f, http.MethodGet,
		"/v1/process/runs/run_maximum_events/events?limit=17", nil)
	assert.Equal(t, http.StatusBadRequest, overMaximum.Code, overMaximum.Body.String())
	assert.Contains(t, overMaximum.Body.String(), `"code":"process_run_limit"`)
}

func TestProcessRuntimeRefusesImplicitProgramAuthorization(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("authorization", 1))
	var dispatched atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		dispatched.Add(1)
		return executor.Execute(ctx, run, dispatch, authorization)
	}))

	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "authorization", "authorizeProgramProfiles": []string{},
	})
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "process_program_unauthorized")
	assert.Zero(t, dispatched.Load())

	duplicate := testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost,
		"/v1/process/runs", strings.NewReader(`{"templateId":"authorization","authorizeProgramProfiles":["safe"],"authorizeProgramProfiles":[]}`))))
	assert.Equal(t, http.StatusBadRequest, duplicate.Code, duplicate.Body.String())
	assert.Zero(t, dispatched.Load(), "duplicate authorization fields must fail closed")

	list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.Contains(t, list.Body.String(), `"runs":[]`)
}

func TestProcessRuntimeColdOutstandingNeedsReconcileWithoutRedispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("cold", 1))
	var attempts atomic.Int32
	prepared := make(chan struct{}, 1)
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		attempts.Add(1)
		prepared <- struct{}{}
		return executor.Result{}, nil, errors.New("simulate daemon loss before dispatch")
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "cold", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	<-prepared
	agentd.WaitForProcessRunRuntimeForTest()
	require.Equal(t, int32(1), attempts.Load())

	// Model a restart: discard every in-memory handle, retain only SQLite, and
	// run the production startup page. The cold outbox item is never dispatched.
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(1), attempts.Load())
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var cold processRuntimeRunView
	testharness.DecodeJSON(t, show, &cold)
	assert.True(t, cold.NeedsReconcile)
	assert.Equal(t, "needs_reconcile", cold.Action)
	require.NotNil(t, cold.Checkpoint.OutstandingCommand)

	invalidResume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/resume", map[string]any{"unexpected": true})
	assert.Equal(t, http.StatusBadRequest, invalidResume.Code, invalidResume.Body.String())
	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/resume", map[string]any{})
	assert.Equal(t, http.StatusConflict, resume.Code, resume.Body.String())
	assert.Contains(t, resume.Body.String(), "process_run_needs_reconcile")
	assert.Equal(t, int32(1), attempts.Load())
}

func TestProcessRuntimeExecutedButUnobservedNeedsReconcileWithoutRedispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("execution-ambiguous", 1))
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER reject_process_observation
		BEFORE UPDATE OF checkpoint_json ON process_runs
		WHEN OLD.id = 'run_execution_ambiguous'
			AND json_type(OLD.checkpoint_json, '$.outstandingCommand') = 'object'
		BEGIN SELECT RAISE(ABORT, 'simulate daemon loss before observation commit'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = database.Exec(`DROP TRIGGER IF EXISTS reject_process_observation`) })

	executed := make(chan executor.Result, 1)
	var attempts atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		attempts.Add(1)
		result, next, executeErr := executor.Execute(ctx, run, dispatch, authorization)
		executed <- result
		return result, next, executeErr
	}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_execution_ambiguous", "templateId": "execution-ambiguous",
		"authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	result := <-executed
	assert.True(t, result.Dispatched, "the external program really ran before the durable observation failed")
	agentd.WaitForProcessRunRuntimeForTest()
	require.Equal(t, int32(1), attempts.Load())
	_, err = database.Exec(`DROP TRIGGER reject_process_observation`)
	require.NoError(t, err)

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	for range 2 {
		agentd.RunProcessRunSweepForTest()
		agentd.WaitForProcessRunRuntimeForTest()
	}
	assert.Equal(t, int32(1), attempts.Load(), "startup never silently redispatches an ambiguous command")
	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_execution_ambiguous", nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var ambiguous processRuntimeRunView
	testharness.DecodeJSON(t, show, &ambiguous)
	assert.Equal(t, "needs_reconcile", ambiguous.Action)
	assert.True(t, ambiguous.NeedsReconcile)

	recorded := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/run_execution_ambiguous/record-outcome", map[string]any{
			"outcome": "succeeded", "exitCode": 0, "note": "external effect confirmed",
		})
	require.Equal(t, http.StatusAccepted, recorded.Code, recorded.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()
	show = processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_execution_ambiguous", nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var completed processRuntimeRunView
	testharness.DecodeJSON(t, show, &completed)
	assert.Equal(t, engine.RunCompleted, completed.Status)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestProcessRuntimeExplicitReissueAndRecordOutcome(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("reconcile", 1))
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		return executor.Result{}, nil, errors.New("leave a cold outstanding command")
	}))

	create := func(id string) processRuntimeRunView {
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": id, "templateId": "reconcile", "authorizeProgramProfiles": []string{"safe"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var view processRuntimeRunView
		testharness.DecodeJSON(t, rec, &view)
		return view
	}
	reissueRun := create("run_reissue")
	recordRun := create("run_record")
	agentd.WaitForProcessRunRuntimeForTest()

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(executor.Execute))
	invalidReissue := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+reissueRun.ID+"/reissue", map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusBadRequest, invalidReissue.Code, invalidReissue.Body.String())
	reissue := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+reissueRun.ID+"/reissue", map[string]any{})
	require.Equal(t, http.StatusAccepted, reissue.Code, reissue.Body.String())
	recorded := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+recordRun.ID+"/record-outcome", map[string]any{
		"outcome": "succeeded", "exitCode": 0, "note": "operator observed success",
	})
	require.Equal(t, http.StatusAccepted, recorded.Code, recorded.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()

	for _, id := range []string{reissueRun.ID, recordRun.ID} {
		show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+id, nil)
		require.Equal(t, http.StatusOK, show.Code, show.Body.String())
		var view processRuntimeRunView
		testharness.DecodeJSON(t, show, &view)
		assert.Equal(t, engine.RunCompleted, view.Status, id)
		assert.False(t, view.NeedsReconcile, id)
	}
}

func TestProcessRuntimeConcurrentReconcileCannotDoubleDispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("reconcile-race", 1))
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		return executor.Result{}, nil, errors.New("leave command ambiguous")
	}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_reconcile_race", "templateId": "reconcile-race",
		"authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())

	entered := make(chan struct{})
	release := make(chan struct{})
	var dispatches atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		if dispatches.Add(1) == 1 {
			close(entered)
		}
		<-release
		return executor.Execute(ctx, run, dispatch, authorization)
	}))

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	requests := make([]*http.Request, 2)
	for i := range requests {
		requests[i] = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/process/runs/run_reconcile_race/reissue", map[string]any{}))
	}
	var wg sync.WaitGroup
	for _, request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- testharness.Serve(f.Mux, request)
		}()
	}
	close(start)
	<-entered
	wg.Wait()
	close(responses)
	codes := make([]int, 0, 2)
	for response := range responses {
		codes = append(codes, response.Code)
	}
	sort.Ints(codes)
	assert.Equal(t, []int{http.StatusAccepted, http.StatusConflict}, codes)
	assert.Equal(t, int32(1), dispatches.Load())
	assert.Equal(t, 1, agentd.ProcessRunClaimCountForTest(), "claim inspection succeeds while execution is blocked")
	close(release)
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(1), dispatches.Load())
}

func TestProcessRuntimeRestartBetweenAtomicCommandsRequiresReconciliation(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("", 2)
	tmpl.Name = "Restart acceptance"
	createdTemplate := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/templates", processEditResponse{Template: tmpl})
	require.Equal(t, http.StatusCreated, createdTemplate.Code, createdTemplate.Body.String())
	var createdAuthoring struct {
		ID         string `json:"id"`
		SourceHash string `json:"sourceHash"`
	}
	testharness.DecodeJSON(t, createdTemplate, &createdAuthoring)
	require.NotEmpty(t, createdAuthoring.ID)
	require.NotEmpty(t, createdAuthoring.SourceHash)

	reopened := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/templates/"+createdAuthoring.ID, nil)
	require.Equal(t, http.StatusOK, reopened.Code, reopened.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, reopened, &edit)
	require.NotNil(t, edit.Template)
	assert.Equal(t, "Restart acceptance", edit.Template.Name)
	edit.Template.Description = "saved before runtime creation"
	saved := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/templates/"+createdAuthoring.ID, edit)
	require.Equal(t, http.StatusCreated, saved.Code, saved.Body.String())

	reopened = processRuntimeRequest(t, f, http.MethodGet, "/v1/process/templates/"+createdAuthoring.ID, nil)
	require.Equal(t, http.StatusOK, reopened.Code, reopened.Body.String())
	testharness.DecodeJSON(t, reopened, &edit)
	assert.Equal(t, "saved before runtime creation", edit.Template.Description)

	var executions atomic.Int32
	firstObserved := make(chan struct{})
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		result, next, err := executor.Execute(ctx, run, dispatch, authorization)
		if executions.Add(1) == 1 && err == nil {
			close(firstObserved)
			return result, next, errors.New("stop daemon after first durable observation")
		}
		return result, next, err
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_fresh_host_restart", "templateId": createdAuthoring.ID,
		"authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	<-firstObserved
	agentd.WaitForProcessRunRuntimeForTest()
	require.Equal(t, int32(1), executions.Load())
	between := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, between.Code, between.Body.String())
	var halfway processRuntimeRunView
	testharness.DecodeJSON(t, between, &halfway)
	assert.Equal(t, engine.RunRunning, halfway.Status)
	assert.Equal(t, "needs_reconcile", halfway.Action)
	assert.Equal(t, engine.NodeDone, halfway.Checkpoint.Nodes["task-01"])
	assert.Equal(t, engine.NodeRunning, halfway.Checkpoint.Nodes["task-02"])
	require.NotNil(t, halfway.Checkpoint.OutstandingCommand)

	// Stop the original daemon-lifetime runtime and re-exec this test binary.
	// The child constructs a new production mux and runtime manager in a new OS
	// process; the only state it shares is HOME (SQLite) and the authoring root.
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	freshView := runProcessRuntimeFreshHost(t, f.World.HomeDir, root, run.ID)
	assert.Equal(t, int32(1), executions.Load(), "the old host never executes the second command")
	assert.Equal(t, engine.RunRunning, freshView.Status)
	assert.Equal(t, "needs_reconcile", freshView.Action)

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var cold processRuntimeRunView
	testharness.DecodeJSON(t, show, &cold)
	assert.Equal(t, engine.RunRunning, cold.Status)
	assert.Equal(t, "needs_reconcile", cold.Action)
}

const processRuntimeFreshHostEnv = "TCLAUDE_TEST_PROCESS_RUNTIME_FRESH_HOST"

func TestProcessRuntimeFreshHostHelper(t *testing.T) {
	if os.Getenv(processRuntimeFreshHostEnv) != "1" {
		t.Skip("fresh-host subprocess helper")
	}
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	t.Cleanup(agentd.SetProcessStoreRootForTest(os.Getenv("TCLAUDE_TEST_PROCESS_STORE_ROOT")))
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(executor.Execute))
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())

	mux := agentd.BuildHandlerForTest()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	rec := testharness.Serve(mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet,
		"/v1/process/runs/"+os.Getenv("TCLAUDE_TEST_PROCESS_RUN_ID"), nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, os.WriteFile(os.Getenv("TCLAUDE_TEST_PROCESS_RESULT"), rec.Body.Bytes(), 0o600))
}

func runProcessRuntimeFreshHost(t *testing.T, home, root, runID string) processRuntimeRunView {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	resultPath := filepath.Join(t.TempDir(), "fresh-host-result.json")
	cmd := osexec.Command(binary, "-test.run=^TestProcessRuntimeFreshHostHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		processRuntimeFreshHostEnv+"=1",
		"HOME="+home,
		"USERPROFILE="+home,
		"TCLAUDE_TEST_PROCESS_STORE_ROOT="+root,
		"TCLAUDE_TEST_PROCESS_RUN_ID="+runID,
		"TCLAUDE_TEST_PROCESS_RESULT="+resultPath,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "fresh host failed: %s", output)
	encoded, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	var view processRuntimeRunView
	require.NoError(t, json.Unmarshal(encoded, &view))
	return view
}

func TestProcessRuntimeConcurrentResumeCannotDoubleDispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("concurrent", 1))
	entered := make(chan struct{})
	release := make(chan struct{})
	var dispatches atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		if dispatches.Add(1) == 1 {
			close(entered)
		}
		<-release
		return executor.Execute(ctx, run, dispatch, authorization)
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "concurrent", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	<-entered

	for range 2 {
		resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/resume", map[string]any{})
		require.Equal(t, http.StatusAccepted, resume.Code, resume.Body.String())
		assert.Contains(t, resume.Body.String(), `"started":false`)
	}
	assert.Equal(t, int32(1), dispatches.Load())
	assert.Equal(t, 1, agentd.ProcessRunClaimCountForTest())
	close(release)
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(1), dispatches.Load())
}

func TestProcessRuntimeCapacityReturnsCreatedRunnableRun(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("capacity", 1))
	release := make(chan struct{})
	entered := make(chan struct{}, db.MaxProcessRunReadPage)
	var dispatches atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		dispatches.Add(1)
		entered <- struct{}{}
		<-release
		return executor.Result{}, nil, errors.New("release capacity test claim")
	}))

	for i := range db.MaxProcessRunReadPage {
		created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": fmt.Sprintf("run_capacity_%02d", i), "templateId": "capacity",
			"authorizeProgramProfiles": []string{"safe"},
		})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	}
	for range db.MaxProcessRunReadPage {
		<-entered
	}
	require.Equal(t, db.MaxProcessRunReadPage, agentd.ProcessRunClaimCountForTest())

	deferred := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_capacity_deferred", "templateId": "capacity",
		"authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, deferred.Code, deferred.Body.String())
	var view processRuntimeRunView
	testharness.DecodeJSON(t, deferred, &view)
	assert.Equal(t, "run_capacity_deferred", view.ID)
	assert.Equal(t, "runnable", view.Action)
	assert.Equal(t, db.MaxProcessRunReadPage, agentd.ProcessRunClaimCountForTest())
	assert.Equal(t, int32(db.MaxProcessRunReadPage), dispatches.Load())

	close(release)
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(db.MaxProcessRunReadPage), dispatches.Load(),
		"the first bounded page advances across the reconciliation-blocked claims")
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(db.MaxProcessRunReadPage+1), dispatches.Load(),
		"the bounded fallback sweep later discovers the capacity-deferred committed run")
}

func TestProcessRuntimeShutdownCancelsAndRecordsActiveDispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("shutdown", 1))
	entered := make(chan struct{})
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		close(entered)
		<-ctx.Done()
		return executor.Execute(ctx, run, dispatch, authorization)
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "shutdown", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	<-entered

	// Reset uses the production shutdown path: cancel every claim, wait for the
	// executor observation to commit, then discard the process-local handles.
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var stopped processRuntimeRunView
	testharness.DecodeJSON(t, show, &stopped)
	assert.Equal(t, engine.RunFailed, stopped.Status)
	assert.Equal(t, "terminal", stopped.Action)
	assert.Nil(t, stopped.Checkpoint.OutstandingCommand)
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())
}

func TestProcessRuntimeShutdownReleasesFullClaimCapacity(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("shutdown-capacity", 1))
	entered := make(chan struct{}, db.MaxProcessRunReadPage)
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, _ *executor.Run, _ *executor.Dispatch, _ executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return executor.Result{}, nil, ctx.Err()
	}))
	for i := range db.MaxProcessRunReadPage {
		created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": fmt.Sprintf("run_shutdown_%02d", i), "templateId": "shutdown-capacity",
			"authorizeProgramProfiles": []string{"safe"},
		})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	}
	for range db.MaxProcessRunReadPage {
		<-entered
	}
	require.Equal(t, db.MaxProcessRunReadPage, agentd.ProcessRunClaimCountForTest())

	remaining, restore, shutdownErr := agentd.ShutdownProcessRunRuntimeForTest()
	t.Cleanup(restore)
	require.NoError(t, shutdownErr)
	assert.Zero(t, remaining, "the stopped manager released every original claim")
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())
	list := processRuntimeRequest(t, f, http.MethodGet,
		fmt.Sprintf("/v1/process/runs?limit=%d", db.MaxProcessRunReadPage), nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var views struct {
		Runs []processRuntimeRunView `json:"runs"`
	}
	testharness.DecodeJSON(t, list, &views)
	require.Len(t, views.Runs, db.MaxProcessRunReadPage)
	for _, run := range views.Runs {
		assert.Equal(t, engine.RunRunning, run.Status, run.ID)
		show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
		require.Equal(t, http.StatusOK, show.Code, show.Body.String())
		var stopped processRuntimeRunView
		testharness.DecodeJSON(t, show, &stopped)
		assert.Equal(t, "needs_reconcile", stopped.Action, run.ID)
		assert.True(t, stopped.NeedsReconcile, run.ID)
		require.NotNil(t, stopped.Checkpoint.OutstandingCommand, run.ID)
	}
}

func TestProcessRuntimeUnsupportedTemplateReturnsClearDiagnostic(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("", 1)
	task := tmpl.Nodes["task-01"]
	task.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Do the work"}
	tmpl.Nodes["task-01"] = task
	createdTemplate := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/templates", processEditResponse{Template: tmpl})
	require.Equal(t, http.StatusCreated, createdTemplate.Code, createdTemplate.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, createdTemplate, &created)

	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": created.ID, "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusUnprocessableEntity, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), `"code":"process_run_invalid"`)
	assert.Contains(t, refused.Body.String(), "nodes.task-01.performer.kind")
	assert.Contains(t, refused.Body.String(), "sequential MVP executes only program performers")
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())
}

func TestProcessRuntimeMalformedAndSemanticallyInvalidCheckpointRefusal(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("checkpoint-refusal", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	createRunnableProcessRunFixture(t, "run_checkpoint_malformed", record.Ref, tmpl)
	createRunnableProcessRunFixture(t, "run_checkpoint_semantic", record.Ref, tmpl)
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`UPDATE process_runs SET checkpoint_json = '{' WHERE id = 'run_checkpoint_malformed'`)
	require.NoError(t, err)
	_, err = database.Exec(`UPDATE process_runs
		SET checkpoint_json = '{"version":1,"runId":"wrong-run","status":"running","nodes":{}}'
		WHERE id = 'run_checkpoint_semantic'`)
	require.NoError(t, err)

	malformed := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_checkpoint_malformed", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, malformed.Code, malformed.Body.String())
	assert.Contains(t, malformed.Body.String(), `"code":"process_run_invalid"`)

	semanticShow := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_checkpoint_semantic", nil)
	require.Equal(t, http.StatusOK, semanticShow.Code, semanticShow.Body.String())
	semanticResume := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/run_checkpoint_semantic/resume", map[string]any{})
	assert.Equal(t, http.StatusUnprocessableEntity, semanticResume.Code, semanticResume.Body.String())
	assert.Contains(t, semanticResume.Body.String(), `"code":"process_run_invalid"`)
	assert.Zero(t, agentd.ProcessRunClaimCountForTest(), "failed cold reconstruction releases its claim")
}

func TestProcessRuntimeFeatureFlagAndPermissionBoundaries(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	off := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	assert.Equal(t, http.StatusNotFound, off.Code, off.Body.String())

	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	const worker = "process-runtime-worker"
	f.HaveEnrolledAgent(worker)
	denied := agentReq(t, f, worker, http.MethodPost, "/v1/process/runs", map[string]any{})
	assert.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	assert.Contains(t, denied.Body.String(), agentd.PermProcessRunsManage)
	readDenied := agentReq(t, f, worker, http.MethodGet, "/v1/process/runs", nil)
	assert.Equal(t, http.StatusForbidden, readDenied.Code, readDenied.Body.String())
	assert.Contains(t, readDenied.Body.String(), agentd.PermProcessRunsRead)
	eventsDenied := agentReq(t, f, worker, http.MethodGet, "/v1/process/runs/run_denied/events", nil)
	assert.Equal(t, http.StatusForbidden, eventsDenied.Code, eventsDenied.Body.String())
	assert.Contains(t, eventsDenied.Body.String(), agentd.PermProcessRunsRead)
	assert.True(t, agentd.IsKnownPermSlug(agentd.PermProcessRunsRead))
	assert.True(t, agentd.IsKnownPermSlug(agentd.PermProcessRunsManage))
}

func TestProcessRuntimeStartupAndFallbackScansOneBoundedPage(t *testing.T) {
	_, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("bounded", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	for i := range db.MaxProcessRunReadPage + 1 {
		createRunnableProcessRunFixture(t, fmt.Sprintf("run_%03d", i), record.Ref, tmpl)
	}
	var dispatches atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		dispatches.Add(1)
		return executor.Result{}, nil, errors.New("stop after the bounded page prepared")
	}))

	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(db.MaxProcessRunReadPage), dispatches.Load())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(db.MaxProcessRunReadPage+1), dispatches.Load())
}

func TestProcessRuntimeReconciliationOnlyPageAdvancesFallbackCursor(t *testing.T) {
	_, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("cursor", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("placeholder", definition)
	require.NoError(t, err)
	checkpoint, err = engine.AdvanceUntilQuiescent(checkpoint, definition)
	require.NoError(t, err)
	require.NotNil(t, checkpoint.OutstandingCommand)

	for i := range db.MaxProcessRunReadPage {
		id := fmt.Sprintf("run_cursor_%02d", i)
		blocked := checkpoint
		blocked.RunID = id
		blocked.OutstandingCommand.ID = id + ":task-01:program"
		createProcessRunFixtureWithCheckpoint(t, id, record.Ref, tmpl, blocked)
	}
	createRunnableProcessRunFixture(t, "run_cursor_zz", record.Ref, tmpl)

	// Make the reconciliation-only rows impossible to reconstruct. The sweep
	// must classify them in SQLite, advance its cursor, and never LoadRun or
	// engine.Prepare them before reaching the runnable row on the next page.
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`UPDATE process_runs SET template_snapshot_json = '{'
		WHERE id >= 'run_cursor_00' AND id <= 'run_cursor_31'`)
	require.NoError(t, err)
	var dispatches atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		dispatches.Add(1)
		return executor.Result{}, nil, errors.New("stop after cursor proof")
	}))

	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Zero(t, dispatches.Load())
	assert.Equal(t, "run_cursor_31", agentd.ProcessRunSweepCursorForTest())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(1), dispatches.Load())
	assert.Empty(t, agentd.ProcessRunSweepCursorForTest())
}

type processRuntimeRunView struct {
	ID                    string            `json:"id"`
	Params                map[string]string `json:"params"`
	ProgramAuthorizations []string          `json:"programAuthorizations"`
	Status                engine.RunStatus  `json:"status"`
	StateVersion          int64             `json:"stateVersion"`
	Action                string            `json:"action"`
	NeedsReconcile        bool              `json:"needsReconcile"`
	Checkpoint            engine.Checkpoint `json:"checkpoint"`
}

type processRuntimeEventPage struct {
	Events []struct {
		Sequence   int64           `json:"sequence"`
		OccurredAt time.Time       `json:"occurredAt"`
		NodeID     string          `json:"nodeId"`
		Kind       string          `json:"kind"`
		Payload    json.RawMessage `json:"payload"`
		Actor      string          `json:"actor"`
	} `json:"events"`
	Next int64 `json:"next"`
}

func processRuntimeFlow(t *testing.T) (*testharness.Flow, string) {
	t.Helper()
	f := newFlow(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	root := filepath.Join(f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	return f, root
}

func processRuntimeTemplate(id string, tasks int) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "task-01"}},
		"end":   {Type: model.NodeTypeEnd, Result: "success"},
	}
	for i := 1; i <= tasks; i++ {
		id := fmt.Sprintf("task-%02d", i)
		next := "end"
		if i < tasks {
			next = fmt.Sprintf("task-%02d", i+1)
		}
		nodes[id] = model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": next},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		}
	}
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start", Nodes: nodes}
}

func putProcessRuntimeTemplate(t *testing.T, root string, tmpl *model.Template) store.TemplateRecord {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	return record
}

func processRuntimeRequest(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, body)))
}

func createRunnableProcessRunFixture(t *testing.T, id, ref string, tmpl *model.Template) {
	t.Helper()
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize(id, definition)
	require.NoError(t, err)
	createProcessRunFixtureWithCheckpoint(t, id, ref, tmpl, checkpoint)
}

func createProcessRunFixtureWithCheckpoint(t *testing.T, id, ref string, tmpl *model.Template, checkpoint engine.Checkpoint) {
	t.Helper()
	createProcessRunFixtureWithEvents(t, id, ref, tmpl, checkpoint, nil)
}

func createProcessRunFixtureWithEvents(t *testing.T, id, ref string, tmpl *model.Template, checkpoint engine.Checkpoint, events []db.ProcessRunEvent) {
	t.Helper()
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	checkpointJSON, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	require.NoError(t, db.CreateProcessRun(db.ProcessRunCreate{
		ID: id, TemplateRef: ref, TemplateSnapshotJSON: snapshot,
		ParamsJSON: json.RawMessage(`{}`), ProgramAuthorizationsJSON: json.RawMessage(`["safe"]`),
		Status: string(checkpoint.Status), CheckpointJSON: checkpointJSON, InitialEvents: events,
	}))
}
