package agentd_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessEngineRoutes404WhenFeatureOff(t *testing.T) {
	f := newFlow(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProcessEngineDynamicallyFollowsFeatureFlag(t *testing.T) {
	f := newFlow(t)
	root := filepath.Join(f.World.HomeDir, ".tclaude", "processes")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	firstOutput := filepath.Join(t.TempDir(), "enabled-output")
	createEngineRun(t, root, "dynamic-enabled", programTemplate("dynamic-enabled", model.Performer{
		Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", `touch "$1"`, "process-test", firstOutput},
	}), true)
	stop := make(chan struct{})
	done := agentd.StartProcessEngineForTest(stop, 5*time.Millisecond)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(stop)
			<-done
		}
	})

	time.Sleep(30 * time.Millisecond)
	_, err := os.Stat(firstOutput)
	assert.ErrorIs(t, err, os.ErrNotExist, "disabled engine must not pick up runs")
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(firstOutput)
		return statErr == nil
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	time.Sleep(30 * time.Millisecond)
	secondOutput := filepath.Join(t.TempDir(), "disabled-output")
	createEngineRun(t, root, "dynamic-disabled", programTemplate("dynamic-disabled", model.Performer{
		Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", `touch "$1"`, "process-test", secondOutput},
	}), true)
	time.Sleep(50 * time.Millisecond)
	_, err = os.Stat(secondOutput)
	assert.ErrorIs(t, err, os.ErrNotExist, "turning the flag off must stop new work")
	close(stop)
	<-done
}

func TestProcessEngineDrivesProgramRunEndToEnd(t *testing.T) {
	f, root := processEngineFlow(t)
	output := filepath.Join(t.TempDir(), "program-count.txt")
	fs := createEngineRun(t, root, "program-run", programTemplate("program", model.Performer{
		Kind: model.PerformerProgram,
		Run:  "/bin/sh",
		Args: []string{"-c", `printf 'ran\n' >> "$1"`, "process-test", output},
	}), true)
	host := processengine.New(fs, "agentd:e2e", map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
	})

	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, "ran\n", string(data))

	rec := processEngineGet(t, f, "/v1/process/runs/program-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		State struct {
			Status state.RunStatus `json:"status"`
		} `json:"state"`
	}
	testharness.DecodeJSON(t, rec, &view)
	assert.Equal(t, state.RunStatusCompleted, view.State.Status)

	counting := &countingProcessStore{Store: fs}
	idleHost := processengine.New(counting, "agentd:terminal-scan", nil)
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), idleHost)
	require.NoError(t, err)
	require.Len(t, results, 1)
	loads, leases := counting.counts()
	assert.Zero(t, loads, "terminal checkpoint must not load evidence")
	assert.Zero(t, leases, "terminal checkpoint must not churn leases")
}

func TestProcessEngineLeaseContentionAllowsOnlyOneScheduler(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := newBlockingAdapter()
	fs := createEngineRun(t, root, "lease-run", programTemplate("lease", model.Performer{Kind: model.PerformerProgram, Run: "/fake"}), true)
	first := processengine.New(fs, "agentd:first", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.LeaseTTL = 150 * time.Millisecond
	second := processengine.New(fs, "agentd:second", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	second.LeaseTTL = 150 * time.Millisecond

	firstDone := make(chan []processengine.RunResult, 1)
	go func() {
		results, _ := agentd.RunProcessEngineTickForTest(t.Context(), first)
		firstDone <- results
	}()
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler never reached performer")
	}
	// Wait past the original TTL: contention must still hold because the
	// first host heartbeats at TTL/3 while its performer is running.
	time.Sleep(2 * first.LeaseTTL)
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].LeaseContended, "result: %+v", results[0])
	assert.Contains(t, results[0].Error, store.ErrLeaseHeld.Error())
	close(adapter.release)
	select {
	case results = <-firstDone:
		require.Len(t, results, 1)
		assert.Empty(t, results[0].Error)
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler did not finish")
	}
}

func TestProcessEngineRestartReconcilesWithoutDoubleExecution(t *testing.T) {
	f, root := processEngineFlow(t)
	adapter := newCrashDiscoverAdapter()
	fs := createEngineRun(t, root, "resume-run", programTemplate("resume", model.Performer{Kind: model.PerformerProgram, Run: "/discoverable"}), true)
	first := processengine.New(fs, "agentd:before-restart", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.Executor.ReconcileDelay = -time.Nanosecond
	ctx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan struct{})
	go func() {
		_, _ = agentd.RunProcessEngineTickForTest(ctx, first)
		close(firstDone)
	}()
	select {
	case <-adapter.performed:
		cancel() // crash after the external result exists, before observation
	case <-time.After(2 * time.Second):
		t.Fatal("performer side effect did not start")
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first host did not stop")
	}

	second := processengine.New(fs, "agentd:after-restart", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	assert.Equal(t, 1, adapter.performCount(), "restart must reconcile, never perform twice")

	rec := processEngineGet(t, f, "/v1/process/runs/resume-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"status":"completed"`)
}

func TestProcessEngineFiresDueTimer(t *testing.T) {
	f, root := processEngineFlow(t)
	fs := createEngineRun(t, root, "timer-run", timerTemplate("timer", time.Minute), false)
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:timer", nil)
	host.Now = func() time.Time { return now }

	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusRunning, results[0].Status)
	snapshot, err := fs.LoadRun(t.Context(), "timer-run")
	require.NoError(t, err)
	require.Len(t, snapshot.State.Timers, 1)
	for _, timer := range snapshot.State.Timers {
		assert.Equal(t, now.Add(time.Minute), timer.DueAt)
		assert.Equal(t, state.WaitStatusPending, timer.Status)
	}

	now = now.Add(2 * time.Minute)
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	rec := processEngineGet(t, f, "/v1/process/runs/timer-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"status":"satisfied"`)
}

func TestProcessEngineRateLimitPauseSurvivesRestartWithoutRetryBudget(t *testing.T) {
	f, root := processEngineFlow(t)
	start := time.Date(2026, 7, 9, 21, 0, 0, 0, time.UTC)
	adapter := &rateLimitThenPassAdapter{until: start.Add(10 * time.Minute)}
	fs := createEngineRun(t, root, "rate-run", programTemplate("rate", model.Performer{Kind: model.PerformerProgram, Run: "/quota"}), true)
	first := processengine.New(fs, "agentd:rate-before", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.Now = func() time.Time { return start }

	results, err := agentd.RunProcessEngineTickForTest(t.Context(), first)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusPaused, results[0].Status)
	assert.Contains(t, results[0].Waiting, "rate limited until")
	snapshot, err := fs.LoadRun(t.Context(), "rate-run")
	require.NoError(t, err)
	require.NotNil(t, snapshot.State.Pause)
	assert.Equal(t, state.PauseKindRateLimited, snapshot.State.Pause.Kind)
	assert.Equal(t, 1, snapshot.State.Nodes["work"].Attempt)

	// A new host reconstructs the pause from state.json, waits through the
	// durable deadline, then retries the same issued command exactly once.
	after := adapter.until.Add(time.Second)
	second := processengine.New(fs, "agentd:rate-after", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	second.Now = func() time.Time { return after }
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	snapshot, err = fs.LoadRun(t.Context(), "rate-run")
	require.NoError(t, err)
	assert.Nil(t, snapshot.State.Pause)
	assert.Equal(t, 1, snapshot.State.Nodes["work"].Attempt, "quota pause must not consume node retry budget")
	assert.Equal(t, 2, adapter.callCount())

	rec := processEngineGet(t, f, "/v1/process/runs/rate-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"pause"`)
}

func TestProcessEngineUndiscoverablePerformerParksNeedsReconcile(t *testing.T) {
	f, root := processEngineFlow(t)
	adapter := &errorAdapter{}
	fs := createEngineRun(t, root, "reconcile-run", programTemplate("reconcile", model.Performer{Kind: model.PerformerProgram, Run: "/unknown-result"}), true)
	first := processengine.New(fs, "agentd:claim", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.Executor.ReconcileDelay = -time.Nanosecond
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), first)
	require.NoError(t, err)

	second := processengine.New(fs, "agentd:resume", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusPaused, results[0].Status)
	assert.Contains(t, results[0].Waiting, "needs reconciliation")
	snapshot, err := fs.LoadRun(t.Context(), "reconcile-run")
	require.NoError(t, err)
	require.NotNil(t, snapshot.State.Pause)
	assert.Equal(t, state.PauseKindNeedsReconcile, snapshot.State.Pause.Kind)
	assert.Equal(t, state.ActorRef("human:operator"), snapshot.State.Pause.Owner)
	assert.Equal(t, 1, snapshot.State.Nodes["work"].Attempt)

	rec := processEngineGet(t, f, "/v1/process/runs/reconcile-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"kind":"needs_reconcile"`)
	assert.Contains(t, rec.Body.String(), `"owner":"human:operator"`)
}

func TestProcessEngineInconsistentRunHaltsWithVisibleReason(t *testing.T) {
	f, root := processEngineFlow(t)
	output := filepath.Join(t.TempDir(), "must-not-run")
	fs := createEngineRun(t, root, "dirty-run", programTemplate("dirty", model.Performer{
		Kind: model.PerformerProgram,
		Run:  "/bin/sh",
		Args: []string{"-c", `touch "$1"`, "process-test", output},
	}), true)
	snapshot, err := fs.LoadRun(t.Context(), "dirty-run")
	require.NoError(t, err)
	node := snapshot.State.Nodes["work"]
	node.Status = state.NodeStatusRunning
	node.ActiveAttempt = nil
	snapshot.State.Nodes["work"] = node
	body, err := state.Encode(snapshot.State)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "dirty-run", "state.json"), body, 0o644))

	host := processengine.New(fs, "agentd:dirty", map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
	})
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusDirty, results[0].Status)
	assert.Contains(t, results[0].Waiting, "running_attempt_without_command_or_actor")
	_, err = os.Stat(output)
	assert.ErrorIs(t, err, os.ErrNotExist)

	rec := processEngineGet(t, f, "/v1/process/runs/dirty-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"effectiveStatus":"dirty"`)
	assert.Contains(t, rec.Body.String(), "running_attempt_without_command_or_actor")
}

func processEngineFlow(t *testing.T) (*testharness.Flow, string) {
	t.Helper()
	f := newFlow(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	root := filepath.Join(f.World.HomeDir, ".tclaude", "processes")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	return f, root
}

func processEngineGet(t *testing.T, f *testharness.Flow, path string) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, path, nil)))
}

func createEngineRun(t *testing.T, root, runID string, tmpl *model.Template, allowPrograms bool) *store.FS {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	templateRecord, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	nodes := make([]state.NodeInit, 0, len(tmpl.Nodes))
	for id, node := range tmpl.Nodes {
		status := state.NodeStatusPending
		if id == tmpl.Start {
			status = state.NodeStatusReady
		}
		nodes = append(nodes, state.NodeInit{ID: id, Type: node.Type, Status: status})
	}
	initial := state.New(runID, templateRecord.Ref, templateRecord.Ref, nodes)
	initial.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: templateRecord.Ref}, initial)
	require.NoError(t, err)
	if allowPrograms {
		at := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)
		_, err = fs.Append(t.Context(), runID, 0, []evidence.LogEntry{{
			SchemaVersion: evidence.LogEntrySchemaVersion,
			At:            at,
			Scope:         evidence.Scope{Kind: evidence.ScopeRun},
			Kind:          evidence.EntryKindAdmin,
			Event: &state.Event{
				Type:   state.EventAdminProgramsAllowed,
				At:     at,
				Actor:  "human:test",
				Reason: "flow test program opt-in",
			},
		}})
		require.NoError(t, err)
		_, err = fs.SetProgramsAllowed(t.Context(), runID)
		require.NoError(t, err)
	}
	return fs
}

func programTemplate(id string, performer model.Performer) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         id,
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &performer, Next: model.Next{"pass": "end"}},
			"end":  {Type: model.NodeTypeEnd},
		},
	}
}

func timerTemplate(id string, duration time.Duration) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         id,
		Start:      "wait",
		Nodes: map[string]model.Node{
			"wait": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: duration.String()}, Next: model.Next{"pass": "end"}},
			"end":  {Type: model.NodeTypeEnd},
		},
	}
}

type blockingAdapter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingAdapter() *blockingAdapter {
	return &blockingAdapter{started: make(chan struct{}), release: make(chan struct{})}
}

func (a *blockingAdapter) Validate(processexec.Request) error { return nil }

func (a *blockingAdapter) Perform(ctx context.Context, _ processexec.Request) (processexec.Observation, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-ctx.Done():
		return processexec.Observation{}, ctx.Err()
	case <-a.release:
		return processexec.Observation{Actor: "program:fake@exit0", Verdict: "pass"}, nil
	}
}

type crashDiscoverAdapter struct {
	mu           sync.Mutex
	observations map[string]processexec.Observation
	count        int
	performed    chan struct{}
	once         sync.Once
}

func newCrashDiscoverAdapter() *crashDiscoverAdapter {
	return &crashDiscoverAdapter{observations: map[string]processexec.Observation{}, performed: make(chan struct{})}
}

func (a *crashDiscoverAdapter) Validate(processexec.Request) error { return nil }

func (a *crashDiscoverAdapter) Perform(ctx context.Context, request processexec.Request) (processexec.Observation, error) {
	observation := processexec.Observation{Actor: "program:discoverable@exit0", Verdict: "pass", ExternalRef: "external:" + request.Command.IdempotencyKey}
	a.mu.Lock()
	a.count++
	a.observations[request.Command.IdempotencyKey] = observation
	a.mu.Unlock()
	a.once.Do(func() { close(a.performed) })
	<-ctx.Done()
	return processexec.Observation{}, ctx.Err()
}

func (a *crashDiscoverAdapter) Reconcile(_ context.Context, request processexec.Request) (processexec.Observation, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	observation, ok := a.observations[request.Command.IdempotencyKey]
	return observation, ok, nil
}

func (a *crashDiscoverAdapter) performCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count
}

type rateLimitThenPassAdapter struct {
	mu    sync.Mutex
	calls int
	until time.Time
}

func (a *rateLimitThenPassAdapter) Validate(processexec.Request) error { return nil }

func (a *rateLimitThenPassAdapter) Perform(_ context.Context, _ processexec.Request) (processexec.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == 1 {
		return processexec.Observation{}, &processexec.RateLimitError{Until: a.until}
	}
	return processexec.Observation{Actor: "program:quota@exit0", Verdict: "pass"}, nil
}

func (a *rateLimitThenPassAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type errorAdapter struct{}

func (a *errorAdapter) Validate(processexec.Request) error { return nil }

func (a *errorAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, errors.New("performer result lost")
}

type countingProcessStore struct {
	store.Store
	mu     sync.Mutex
	loads  int
	leases int
}

func (s *countingProcessStore) LoadRun(ctx context.Context, runID string) (store.Snapshot, error) {
	s.mu.Lock()
	s.loads++
	s.mu.Unlock()
	return s.Store.LoadRun(ctx, runID)
}

func (s *countingProcessStore) AcquireRunLease(ctx context.Context, runID, holder string, ttl time.Duration) (store.LeaseRecord, error) {
	s.mu.Lock()
	s.leases++
	s.mu.Unlock()
	return s.Store.AcquireRunLease(ctx, runID, holder, ttl)
}

func (s *countingProcessStore) counts() (loads, leases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads, s.leases
}
