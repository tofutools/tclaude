package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, error) {
		result, err := executor.Execute(ctx, run, dispatch, authorization)
		completed <- struct{}{}
		return result, err
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
	assert.Nil(t, shown.Checkpoint.OutstandingCommand)
	assert.Equal(t, "terminal", shown.Action)
	assert.Equal(t, map[string]string{"branch": "main"}, shown.Params)
	assert.Equal(t, []string{"safe"}, shown.ProgramAuthorizations)

	events, err := db.ListProcessRunEvents(createdRun.ID, 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	assert.Equal(t, []string{"run_created", "program_prepared", "program_observed", "program_prepared", "program_observed", "engine_advanced"}, kinds)
}

func TestProcessRuntimeRefusesImplicitProgramAuthorization(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("authorization", 1))
	var dispatched atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, error) {
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
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, error) {
		attempts.Add(1)
		prepared <- struct{}{}
		return executor.Result{}, errors.New("simulate daemon loss before dispatch")
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

	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/resume", map[string]any{})
	assert.Equal(t, http.StatusConflict, resume.Code, resume.Body.String())
	assert.Contains(t, resume.Body.String(), "process_run_needs_reconcile")
	assert.Equal(t, int32(1), attempts.Load())
}

func TestProcessRuntimeExplicitReissueAndRecordOutcome(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("reconcile", 1))
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, error) {
		return executor.Result{}, errors.New("leave a cold outstanding command")
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

func TestProcessRuntimeRestartBetweenCommandsContinuesAutomatically(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("restart", 2))
	var executions atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, error) {
		result, err := executor.Execute(ctx, run, dispatch, authorization)
		if executions.Add(1) == 1 && err == nil {
			return result, errors.New("stop daemon after first durable observation")
		}
		return result, err
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "restart", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	agentd.WaitForProcessRunRuntimeForTest()
	require.Equal(t, int32(1), executions.Load())

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(2), executions.Load())
	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var completed processRuntimeRunView
	testharness.DecodeJSON(t, show, &completed)
	assert.Equal(t, engine.RunCompleted, completed.Status)
}

func TestProcessRuntimeConcurrentResumeCannotDoubleDispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("concurrent", 1))
	entered := make(chan struct{})
	release := make(chan struct{})
	var dispatches atomic.Int32
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, error) {
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

func TestProcessRuntimeShutdownCancelsAndRecordsActiveDispatch(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("shutdown", 1))
	entered := make(chan struct{})
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, error) {
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
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(context.Context, *executor.Run, *executor.Dispatch, executor.Authorization) (executor.Result, error) {
		dispatches.Add(1)
		return executor.Result{}, errors.New("stop after the bounded page prepared")
	}))

	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(db.MaxProcessRunReadPage), dispatches.Load())
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, int32(db.MaxProcessRunReadPage+1), dispatches.Load())
}

type processRuntimeRunView struct {
	ID                    string            `json:"id"`
	Params                map[string]string `json:"params"`
	ProgramAuthorizations []string          `json:"programAuthorizations"`
	Status                engine.RunStatus  `json:"status"`
	Action                string            `json:"action"`
	NeedsReconcile        bool              `json:"needsReconcile"`
	Checkpoint            engine.Checkpoint `json:"checkpoint"`
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
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	checkpointJSON, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	require.NoError(t, db.CreateProcessRun(db.ProcessRunCreate{
		ID: id, TemplateRef: ref, TemplateSnapshotJSON: snapshot,
		ParamsJSON: json.RawMessage(`{}`), ProgramAuthorizationsJSON: json.RawMessage(`["safe"]`),
		Status: string(checkpoint.Status), CheckpointJSON: checkpointJSON,
	}))
}
