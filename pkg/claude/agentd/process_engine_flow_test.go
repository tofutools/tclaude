package agentd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
	"github.com/tofutools/tclaude/pkg/claude/processcmd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessEngineRoutes404WhenFeatureOff(t *testing.T) {
	f := newFlow(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateRef": "off@sha256:" + strings.Repeat("0", 64),
	})))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/worklist", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/process/worklist/wi_missing/action", map[string]string{
		"action": "approve", "comment": "reviewed", "idempotencyKey": "off-1",
	})))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProcessRunCreatePinsExactRefAppliesDefaultsAndInterpolates(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	required := true
	v1 := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "rest-instantiate", Start: "work",
		Params: map[string]model.Param{
			"issue": {Type: "string", Required: &required, Description: "Issue id"},
			"tries": {Type: "number", Default: 2},
		},
		Nodes: map[string]model.Node{
			"work": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Implement {{ params.issue }} in {{ params.tries }} passes"},
				Next:      model.Next{"pass": "done"},
			},
			"done": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	v1Record, err := fs.PutTemplate(t.Context(), v1)
	require.NoError(t, err)
	v2 := *v1
	v2.Nodes = maps.Clone(v1.Nodes)
	work := v2.Nodes["work"]
	work.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "WRONG HEAD {{ params.issue }}"}
	v2.Nodes["work"] = work
	v2Record, err := fs.PutTemplate(t.Context(), &v2)
	require.NoError(t, err)
	require.NotEqual(t, v1Record.Ref, v2Record.Ref)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateRef": v1Record.Ref,
		"runId":       "rest-exact-run",
		"params":      map[string]string{"issue": "TCL-300"},
	})))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "/v1/process/runs/rest-exact-run/view", rec.Header().Get("Location"))
	var created struct {
		Run struct {
			ID          string    `json:"id"`
			TemplateRef string    `json:"templateRef"`
			CreatedAt   time.Time `json:"createdAt"`
			UpdatedAt   time.Time `json:"updatedAt"`
		} `json:"run"`
	}
	testharness.DecodeJSON(t, rec, &created)
	assert.Equal(t, "rest-exact-run", created.Run.ID)
	assert.Equal(t, v1Record.Ref, created.Run.TemplateRef)
	assert.NotZero(t, created.Run.CreatedAt)
	assert.NotZero(t, created.Run.UpdatedAt)
	var envelope map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	createdKeys := make([]string, 0, len(envelope["run"]))
	for key := range envelope["run"] {
		createdKeys = append(createdKeys, key)
	}
	assert.ElementsMatch(t, []string{"id", "templateRef", "createdAt", "updatedAt"}, createdKeys)

	run, err := fs.GetRun(t.Context(), "rest-exact-run")
	require.NoError(t, err)
	assert.Equal(t, v1Record.Ref, run.TemplateRef)
	assert.Equal(t, map[string]string{"issue": "TCL-300", "tries": "2"}, run.Params)
	assert.False(t, run.AllowPrograms, "the REST surface must not opt runs into local program execution")
	require.NotNil(t, run.Template)
	assert.Equal(t, v1.Nodes["work"].Performer.Prompt, run.Template.Nodes["work"].Performer.Prompt)

	adapter := &captureInstantiateAdapter{}
	host := processengine.New(fs, "agentd:instantiate-flow", map[model.PerformerKind]processexec.Adapter{
		model.PerformerAgent: adapter,
	})
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	request := adapter.request()
	assert.Equal(t, "Implement TCL-300 in 2 passes", request.Performer.Prompt)
}

func TestProcessRunCreateUsesDedicatedPermission(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "permission-run", Start: "done",
		Params: map[string]model.Param{"secret": {Type: "string"}},
		Nodes:  map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	assert.True(t, agentd.IsKnownPermSlug(agentd.PermProcessRunsCreate))
	const conv = "process-run-caller-aaaa-bbbb"
	const secret = "audit-must-not-record-this-param"
	body := map[string]any{"templateRef": record.Ref, "runId": "permission-created", "params": map[string]string{"secret": secret}}

	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermProcessTemplatesManage, "test"))
	denied := agentReq(t, f, conv, http.MethodPost, "/v1/process/runs", body)
	assert.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	assert.Contains(t, denied.Body.String(), agentd.PermProcessRunsCreate)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermProcessRunsCreate, "test"))
	created := agentReq(t, f, conv, http.MethodPost, "/v1/process/runs", body)
	assert.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "process.run.create"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "both denied and successful durable command attempts are audited")
	statuses := map[int]bool{}
	for _, row := range rows {
		statuses[row.Status] = true
		assert.Equal(t, http.MethodPost, row.Method)
		assert.Equal(t, "/v1/process/runs", row.Path)
		assert.Equal(t, db.AuditSourceCLI, row.Source)
		assert.Empty(t, row.Detail, "nil describer must not buffer runtime params")
		assert.NotContains(t, row.Detail, secret)
	}
	assert.True(t, statuses[http.StatusForbidden], "denied attempt missing from audit")
	assert.True(t, statuses[http.StatusCreated], "successful attempt missing from audit")
}

func TestProcessRunCreateDashboardAuditAttribution(t *testing.T) {
	_, root := processEngineFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "dashboard-audit-run", Start: "done",
		Params: map[string]model.Param{"secret": {Type: "string"}},
		Nodes:  map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	const secret = "dashboard-audit-must-not-record-this-param"

	dashboard := agentd.BuildDashboardHandlerForTest()
	created := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateRef": record.Ref,
		"runId":       "dashboard-audit-created",
		"params":      map[string]string{"secret": secret},
	}))
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "process.run.create"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, db.AuditActorHuman, row.ActorKind)
	assert.Equal(t, "operator", row.ActorLabel)
	assert.Equal(t, db.AuditSourceDashboard, row.Source)
	assert.Equal(t, http.MethodPost, row.Method)
	assert.Equal(t, "/v1/process/runs", row.Path)
	assert.Equal(t, http.StatusCreated, row.Status)
	assert.Empty(t, row.Detail, "nil describer must not buffer runtime params")
	assert.NotContains(t, row.Detail, secret)
}

func TestProcessRunCreateStrictSanitizedBoundary(t *testing.T) {
	f, _ := processEngineFlow(t)
	serveRaw := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/process/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
	}
	for name, body := range map[string]string{
		"unknown field":    `{"templateRef":"x","superSecretField":true}`,
		"non-string param": `{"templateRef":"x","params":{"tries":2}}`,
		"trailing json":    `{"templateRef":"x"}{"templateRef":"y"}`,
		"oversized":        `{"templateRef":"` + strings.Repeat("x", (4<<20)+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveRaw(body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "superSecretField")
		})
	}
	secret := "../../home/operator/private-template"
	invalid := serveRaw(`{"templateRef":"` + secret + `"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, invalid.Code, invalid.Body.String())
	assert.NotContains(t, invalid.Body.String(), secret)
	assert.Contains(t, invalid.Body.String(), "exact content-addressed")
	missing := serveRaw(`{"templateRef":"missing@sha256:` + strings.Repeat("a", 64) + `"}`)
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
}

func TestProcessRunCreateRejectsInvalidEditorSavedVersionWithoutRun(t *testing.T) {
	f, root := processEngineFlow(t)
	tmpl := processRESTTemplate("invalid-run-source", "advisory editor draft", 10)
	edges := model.NormalizeEdges(tmpl)
	for i := range edges {
		if edges[i].From == "begin" {
			edges[i].To = "missing"
		}
	}
	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/invalid-run-source", processEditResponse{
		Template: semanticProcessTemplate(tmpl), Edges: edges, Layout: tmpl.Layout,
	})
	require.Equal(t, http.StatusCreated, save.Code, save.Body.String())
	var saved struct {
		Ref         string            `json:"ref"`
		Diagnostics []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, save, &saved)
	require.NotEmpty(t, saved.Ref)
	require.NotEmpty(t, saved.Diagnostics, "editor saves keep validation errors as an advisory draft")

	create := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateRef": saved.Ref, "runId": "invalid-editor-run",
	})))
	assert.Equal(t, http.StatusUnprocessableEntity, create.Code, create.Body.String())
	assert.Contains(t, create.Body.String(), "template, runId, or params are invalid")
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	_, err = fs.GetRun(t.Context(), "invalid-editor-run")
	assert.ErrorIs(t, err, store.ErrNotFound)
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
	pulses, observed, done := agentd.StartProcessEngineForTest(stop)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(stop)
			<-done
		}
	})

	observeProcessEngineState(t, observed, false)
	_, err := os.Stat(firstOutput)
	assert.ErrorIs(t, err, os.ErrNotExist, "disabled engine must not pick up runs")
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	pulseProcessEngine(t, pulses, observed, true)
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(firstOutput)
		return statErr == nil
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	pulseProcessEngine(t, pulses, observed, false)
	secondOutput := filepath.Join(t.TempDir(), "disabled-output")
	createEngineRun(t, root, "dynamic-disabled", programTemplate("dynamic-disabled", model.Performer{
		Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", `touch "$1"`, "process-test", secondOutput},
	}), true)
	// Process one complete supervisor cycle with the second run present. The
	// false observation proves the cycle was consumed while disabled.
	pulseProcessEngine(t, pulses, observed, false)
	_, err = os.Stat(secondOutput)
	assert.ErrorIs(t, err, os.ErrNotExist, "turning the flag off must stop new work")
	close(stop)
	<-done
}

func pulseProcessEngine(t *testing.T, pulses chan<- struct{}, observed <-chan bool, want bool) {
	t.Helper()
	select {
	case pulses <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending process engine supervisor pulse")
	}
	observeProcessEngineState(t, observed, want)
}

func observeProcessEngineState(t *testing.T, observed <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-observed:
		assert.Equal(t, want, got, "process engine observed unexpected feature state")
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for process engine to observe enabled=%v", want)
	}
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

func TestProcessEngineDrivesCodeChangeStrawmanHappyPath(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, marker := createCapstoneRun(t, root, "capstone-happy", 1)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneTickWaiting(t, host, "implement.plan")
	capstoneReportAgent(t, f, fs, "capstone-happy", "implement.plan", "artifact:plan")
	capstoneTickWaiting(t, host, "implement.plan.approval")
	capstoneReplyHuman(t, fs, "capstone-happy", "implement.plan.approval", "approve plan reviewed")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-happy", "implement.do", "commit:happy")

	// The real hermetic program check executes inside this tick, after which
	// the engine advances unattended to the agent cold-review obligation.
	capstoneTickWaiting(t, host, "implement.test.cold-review")
	capstoneReportAgent(t, f, fs, "capstone-happy", "implement.test.cold-review", "review:happy")
	capstoneTickWaiting(t, host, "implement.review")
	capstoneReplyHuman(t, fs, "capstone-happy", "implement.review", "approve merge")
	result := capstoneTick(t, host)
	assert.Equal(t, state.RunStatusCompleted, result.Status)

	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(data))
	assertCapstoneAuditableFromRunDir(t, root, "capstone-happy")
}

func TestProcessEngineCodeChangePoisonDecisionRetrySurvivesRestart(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, marker := createCapstoneRun(t, root, "capstone-retry", 3)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-retry")
	capstoneReportAgent(t, f, fs, "capstone-retry", "implement.do", "commit:retry-1")
	capstoneTickWaiting(t, host, "implement.do") // first check failure feeds back
	capstoneReportAgent(t, f, fs, "capstone-retry", "implement.do", "commit:retry-2")
	capstoneTickWaiting(t, host, "escalate") // second failure poisons, then offers the decision

	blocked, err := fs.LoadRun(t.Context(), "capstone-retry")
	require.NoError(t, err)
	assert.Equal(t, state.NodeStatusBlocked, blocked.State.Nodes["implement"].Status)
	assert.Equal(t, state.NodeStatusBlocked, blocked.State.Nodes["implement.test.tests"].Status)
	assert.Contains(t, blocked.State.Nodes["implement"].BlockedReason, "exhausted its budget of 2 failed verdicts")
	capstoneReplyHuman(t, fs, "capstone-retry", "escalate", "retry transient failure reviewed")

	// Simulate daemon death immediately after the planner/executor has durably
	// claimed resolve_block, before it can append the audited resolution.
	ctx, cancel := context.WithCancel(t.Context())
	crashStore := &cancelAfterResolveClaimStore{Store: fs, cancel: cancel}
	beforeRestart := processengine.New(crashStore, "agentd:capstone-before-restart", nil)
	results, err := agentd.RunProcessEngineTickForTest(ctx, beforeRestart)
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	} else {
		require.Len(t, results, 1)
		assert.Contains(t, results[0].Error, context.Canceled.Error())
	}
	claimed, err := fs.LoadRun(t.Context(), "capstone-retry")
	require.NoError(t, err)
	resolveID := outstandingCommandForNode(t, claimed.State, "escalate", state.CommandKindResolveBlock)
	assert.Equal(t, state.CommandStatusIssued, claimed.State.OutstandingCommands[resolveID].Status)
	assert.Equal(t, state.NodeStatusBlocked, claimed.State.Nodes["implement"].Status)

	// A fresh production host rediscovers the claimed command, applies the
	// existing ResolveBlocked funnel exactly once, and continues into attempt 3.
	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	capstoneTickWaiting(t, restarted, "implement.test.cold-review")
	resolved, err := fs.LoadRun(t.Context(), "capstone-retry")
	require.NoError(t, err)
	assert.Equal(t, state.CommandStatusObserved, resolved.State.OutstandingCommands[resolveID].Status)
	assert.Equal(t, 1, blockResolutionCount(resolved.State))
	assert.Equal(t, state.BlockDecisionRetry, resolved.State.Nodes["implement"].BlockResolution.Decision)

	capstoneReportAgent(t, f, fs, "capstone-retry", "implement.test.cold-review", "review:retry")
	capstoneTickWaiting(t, restarted, "implement.review")
	capstoneReplyHuman(t, fs, "capstone-retry", "implement.review", "approve merge")
	result := capstoneTick(t, restarted)
	assert.Equal(t, state.RunStatusCompleted, result.Status)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "3\n", string(data), "restart must not execute a program idempotency key twice")
	assertCapstoneAuditableFromRunDir(t, root, "capstone-retry")
}

func TestProcessEngineCodeChangePoisonDecisionCancel(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, marker := createCapstoneRun(t, root, "capstone-cancel", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-cancel")
	capstoneReportAgent(t, f, fs, "capstone-cancel", "implement.do", "commit:cancel-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-cancel", "implement.do", "commit:cancel-2")
	capstoneTickWaiting(t, host, "escalate")
	capstoneReplyHuman(t, fs, "capstone-cancel", "escalate", "cancel do not merge")

	ctx, cancel := context.WithCancel(t.Context())
	crashStore := &cancelAfterResolveClaimStore{Store: fs, cancel: cancel}
	beforeRestart := processengine.New(crashStore, "agentd:cancel-before-restart", nil)
	results, err := agentd.RunProcessEngineTickForTest(ctx, beforeRestart)
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	} else {
		require.Len(t, results, 1)
		assert.Contains(t, results[0].Error, context.Canceled.Error())
	}
	claimed, err := fs.LoadRun(t.Context(), "capstone-cancel")
	require.NoError(t, err)
	resolveID := outstandingCommandForNode(t, claimed.State, "escalate", state.CommandKindResolveBlock)
	assert.Equal(t, state.NodeStatusBlocked, claimed.State.Nodes["implement"].Status)
	assert.Equal(t, state.NodeStatusPending, claimed.State.Nodes["canceled"].Status)

	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	result := capstoneTick(t, restarted)
	assert.Equal(t, state.RunStatusCanceled, result.Status)

	snapshot, err := fs.LoadRun(t.Context(), "capstone-cancel")
	require.NoError(t, err)
	require.NotNil(t, snapshot.State.Nodes["implement"].BlockResolution)
	assert.Equal(t, state.BlockDecisionCancel, snapshot.State.Nodes["implement"].BlockResolution.Decision)
	assert.Equal(t, 1, blockResolutionCount(snapshot.State))
	assert.Equal(t, state.CommandStatusObserved, snapshot.State.OutstandingCommands[resolveID].Status)
	var cancelCommand state.OutstandingCommand
	for _, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindResolveBlock {
			cancelCommand = command
		}
	}
	assert.Equal(t, state.CommandStatusObserved, cancelCommand.Status, "cancel must atomically close its resolve command before the run becomes terminal")
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "2\n", string(data))
	assertCapstoneAuditableFromRunDir(t, root, "capstone-cancel")
}

func TestProcessEngineClosesClaimedResolutionSupersededByManualCancel(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, _ := createCapstoneRun(t, root, "capstone-superseded", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-superseded")
	capstoneReportAgent(t, f, fs, "capstone-superseded", "implement.do", "commit:superseded-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-superseded", "implement.do", "commit:superseded-2")
	capstoneTickWaiting(t, host, "escalate")
	capstoneReplyHuman(t, fs, "capstone-superseded", "escalate", "retry reviewed")

	ctx, cancel := context.WithCancel(t.Context())
	crashStore := &cancelAfterResolveClaimStore{Store: fs, cancel: cancel}
	beforeRestart := processengine.New(crashStore, "agentd:superseded-before-restart", nil)
	_, _ = agentd.RunProcessEngineTickForTest(ctx, beforeRestart)
	claimed, err := fs.LoadRun(t.Context(), "capstone-superseded")
	require.NoError(t, err)
	resolveID := outstandingCommandForNode(t, claimed.State, "escalate", state.CommandKindResolveBlock)
	assert.Equal(t, state.CommandStatusIssued, claimed.State.OutstandingCommands[resolveID].Status)

	executor := processexec.New(fs, nil)
	_, err = executor.ResolveBlocked(t.Context(), processexec.BlockResolutionRequest{
		RunID: "capstone-superseded", NodeID: "implement.test.tests", BlockedAttempt: 2,
		Decision: state.BlockDecisionCancel, Actor: "human:operator", Reason: "operator canceled during restart",
		EvidenceRef: "human-message:manual-cancel",
	})
	require.NoError(t, err)

	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	result := capstoneTick(t, restarted)
	assert.Equal(t, state.RunStatusCanceled, result.Status)
	assert.Empty(t, result.Error)
	closed, err := fs.LoadRun(t.Context(), "capstone-superseded")
	require.NoError(t, err)
	assert.Equal(t, state.CommandStatusObserved, closed.State.OutstandingCommands[resolveID].Status)
	assert.Equal(t, "superseded", closed.State.OutstandingCommands[resolveID].Verdict)
}

func TestProcessEngineConsumedDecisionDoesNotRetryLaterPoison(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, _ := createCapstoneRun(t, root, "capstone-repoison", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-repoison")
	capstoneReportAgent(t, f, fs, "capstone-repoison", "implement.do", "commit:repoison-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-repoison", "implement.do", "commit:repoison-2")
	capstoneTickWaiting(t, host, "escalate")
	capstoneReplyHuman(t, fs, "capstone-repoison", "escalate", "retry reviewed once")

	// The released check fails once within its fresh budget, feeds back into
	// the last allowed do attempt, then fails again into a later poison.
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-repoison", "implement.do", "commit:repoison-3")
	result := capstoneTick(t, host)
	assert.Equal(t, state.RunStatusRunning, result.Status)
	snapshot, err := fs.LoadRun(t.Context(), "capstone-repoison")
	require.NoError(t, err)
	assert.Equal(t, state.NodeStatusBlocked, snapshot.State.Nodes["implement"].Status)
	assert.Equal(t, state.NodeStatusCompleted, snapshot.State.Nodes["escalate"].Status)
	assert.Equal(t, 1, blockResolutionCount(snapshot.State), "old human decision must not resolve a later poison")
	for _, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindResolveBlock && command.Status == state.CommandStatusIssued {
			t.Fatalf("old decision emitted a fresh resolution: %#v", command)
		}
	}
}

func TestProcessEngineAgentSpawnReportSettleAndResumeSuppression(t *testing.T) {
	f, root := processEngineFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "process-dev", Harness: "claude", Model: "haiku", Effort: "low",
	})
	require.NoError(t, err)
	fs := createEngineRun(t, root, "agent-run", programTemplate("agent-process", model.Performer{
		Kind: model.PerformerAgent, Profile: "process-dev", Model: "opus", Effort: "high",
		Prompt:  "Implement the requested change",
		Contact: &model.ContactSchedule{Cadence: "1h", Budget: 2, EscalationTarget: "human:operator"},
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)

	snapshot, err := fs.LoadRun(t.Context(), "agent-run")
	require.NoError(t, err)
	require.Len(t, snapshot.State.Obligations, 1)
	require.Len(t, snapshot.State.Contacts, 1)
	var commandID string
	for id, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindStartAttempt {
			commandID = id
			assert.NotEmpty(t, command.ExternalRef)
		}
	}
	require.NotEmpty(t, commandID)
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	spawnModel, ok := f.World.SpawnModel(agentRow.CurrentConvID)
	require.True(t, ok)
	assert.Equal(t, "opus", spawnModel, "performer model overrides the named profile")
	spawnEffort, ok := f.World.SpawnEffort(agentRow.CurrentConvID)
	require.True(t, ok)
	assert.Equal(t, "high", spawnEffort, "performer effort overrides the named profile")
	firstAgentID := agentRow.AgentID
	assert.Equal(t, firstAgentID, snapshot.State.OutstandingCommands[commandID].ExternalRef)
	assert.Equal(t, "agent:"+firstAgentID, firstObligation(snapshot.State).Assignee)
	assert.Equal(t, "agent:"+firstAgentID, snapshot.State.Contacts[commandID].Assignee)
	assert.NotEqual(t, agentRow.CurrentConvID, snapshot.State.OutstandingCommands[commandID].ExternalRef)

	// A fresh host rediscovers the metadata-bound actor and leaves it in
	// flight; it must not dispatch a second agent.
	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Waiting, "waiting on agent:")
	agentRow, err = db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	assert.Equal(t, firstAgentID, agentRow.AgentID)

	reportBody := map[string]string{
		"command_id": commandID, "verdict": "pass", "evidence_ref": "commit:abc123",
	}
	foreignConv := "ffff-1111-2222-3333-444444444444"
	_, _, err = db.EnsureAgentForConv(foreignConv, "test")
	require.NoError(t, err)
	denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/agent-run/nodes/work/report", reportBody), foreignConv))
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	invalidBody := map[string]string{
		"command_id": commandID, "verdict": "approve", "evidence_ref": "commit:invalid",
	}
	invalid := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/agent-run/nodes/work/report", invalidBody), agentRow.CurrentConvID))
	require.Equal(t, http.StatusConflict, invalid.Code, invalid.Body.String())
	assert.Contains(t, invalid.Body.String(), "allowed: pass, fail, ask-changes")

	req := testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs/agent-run/nodes/work/report", reportBody)
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, agentRow.CurrentConvID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	settled, err := fs.LoadRun(t.Context(), "agent-run")
	require.NoError(t, err)
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(settled.State).Status)
	assert.Equal(t, state.ActorRef("agent:"+firstAgentID), settled.State.Nodes["work"].ActiveAttempt.Actor)
}

func TestProcessEngineInvalidAgentOverrideFailsBeforeCommandClaim(t *testing.T) {
	_, root := processEngineFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "process-valid", Harness: "claude"})
	require.NoError(t, err)
	fs := createEngineRun(t, root, "invalid-agent-override", programTemplate("invalid-agent-override", model.Performer{
		Kind: model.PerformerAgent, Profile: "process-valid", Model: "not-a-model", Prompt: "work",
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Error, "not-a-model")

	snapshot, err := fs.LoadRun(t.Context(), "invalid-agent-override")
	require.NoError(t, err)
	assert.Empty(t, snapshot.State.OutstandingCommands, "deterministic validation must fail before CommandIssued")
	assert.Empty(t, snapshot.State.Obligations)
	assert.Empty(t, snapshot.State.Contacts)
	work := snapshot.State.Nodes["work"]
	assert.Zero(t, work.Attempt)
	assert.Nil(t, work.ActiveAttempt, "NodeAttemptStarted must not be recorded")
}

func TestProcessEngineOwnInboxDeliveryDoesNotPreemptAgent(t *testing.T) {
	_, root := processEngineFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "process-preempt", Harness: "claude"})
	require.NoError(t, err)
	fs := createEngineRun(t, root, "agent-delivery-run", programTemplate("agent-delivery", model.Performer{
		Kind: model.PerformerAgent, Profile: "process-preempt", Prompt: "Implement the requested change",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 2, EscalationTarget: "human:operator"},
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	snapshot, err := fs.LoadRun(t.Context(), "agent-delivery-run")
	require.NoError(t, err)
	commandID := firstContact(snapshot.State).CommandID
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: 0, ToConv: agentRow.CurrentConvID, Subject: "Process nudge", Body: "continue",
		ToRecipients: []string{agentRow.CurrentConvID},
	})
	require.NoError(t, err)
	require.NoError(t, db.MarkAgentMessageDelivered(messageID))
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)
	require.NotNil(t, message)
	require.False(t, message.DeliveredAt.IsZero())

	sessionRow, err := db.FindSessionByConvID(agentRow.CurrentConvID)
	require.NoError(t, err)
	require.NotNil(t, sessionRow)
	sessionRow.StatusDetail = "UserPromptSubmit"
	sessionRow.LastHook = message.DeliveredAt
	require.NoError(t, db.SaveSession(sessionRow))
	host.Now = func() time.Time { return message.DeliveredAt.Add(6 * time.Second) }
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	snapshot, err = fs.LoadRun(t.Context(), "agent-delivery-run")
	require.NoError(t, err)
	contact := firstContact(snapshot.State)
	assert.False(t, contact.Paused)
	assert.True(t, contact.HumanInteractedAt.IsZero())
	assert.Equal(t, 1, contact.Used, "the due nudge proceeds after an automated UserPromptSubmit")
}

func TestProcessEngineHumanObligationAppearsAndResolvesThroughCLI(t *testing.T) {
	_, root := processEngineFlow(t)
	fs := createEngineRun(t, root, "human-run", programTemplate("human-process", model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Assignee: "johan", Ask: "Approve the release?",
		Contact: &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:operator"},
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "human-run")
	require.NoError(t, err)
	obligation := firstObligation(snapshot.State)
	assert.Equal(t, state.WaitStatusPending, obligation.Status)
	assert.Equal(t, "human:johan", obligation.Assignee)
	assert.Equal(t, []string{"approve", "reject", "ask-changes"}, obligation.AvailableActions)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[0].Body, "Approve the release?")

	cmd := processcmd.Cmd()
	cmd.SetArgs([]string{"resolve", "human-run", "work", "--store-root", root, "--verdict", "approve", "--actor", "human:johan", "--evidence", "approval:dashboard-1"})
	require.NoError(t, cmd.Execute())
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	settled, err := fs.LoadRun(t.Context(), "human-run")
	require.NoError(t, err)
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(settled.State).Status)
}

func TestProcessEngineHumanCustomChoicesRouteWorkCheckAndReview(t *testing.T) {
	f, root := processEngineFlow(t)
	human := func(ask string, choices []string, outcomes map[string]string) model.Performer {
		return model.Performer{
			Kind: model.PerformerHuman, Profile: "operator", Ask: ask,
			Choices: choices, ChoiceOutcomes: outcomes,
		}
	}
	doPerformer := human("Finish work?", []string{"ship", "hold"}, map[string]string{"ship": "pass", "hold": "fail"})
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "custom-choice-stages", Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {
				Type:      model.NodeTypeTask,
				Performer: &doPerformer,
				Checks:    []model.Step{{ID: "quality", Performer: human("Quality result?", []string{"green", "red"}, map[string]string{"green": "pass", "red": "fail"})}},
				Review:    &model.Step{ID: "review", Performer: human("Review result?", []string{"merge", "revise"}, map[string]string{"merge": "pass", "revise": "fail"})},
				Next:      model.Next{"pass": "done"},
			},
			"done": {Type: model.NodeTypeEnd},
		},
	}
	fs := createEngineRun(t, root, "custom-choice-run", tmpl, false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	resolve := func(nodeID, choice string, expected []string) {
		capstoneTickWaiting(t, host, nodeID)
		snapshot, loadErr := fs.LoadRun(t.Context(), "custom-choice-run")
		require.NoError(t, loadErr)
		obligation := firstObligationForNode(snapshot.State, nodeID)
		assert.Equal(t, expected, obligation.AvailableActions)
		capstoneReplyHuman(t, fs, "custom-choice-run", nodeID, choice+" reviewed")
	}

	// A custom vocabulary is closed: the hidden legacy pass alias must not be
	// accepted when it was not authored.
	capstoneTickWaiting(t, host, "implement.do")
	snapshot, err := fs.LoadRun(t.Context(), "custom-choice-run")
	require.NoError(t, err)
	listingRec := processEngineGet(t, f, "/v1/process/worklist?status=pending")
	require.Equal(t, http.StatusOK, listingRec.Code, listingRec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, listingRec, &listing)
	require.Len(t, listing.Items, 1)
	item := listing.Items[0]
	require.Equal(t, "implement.do", item.Node)
	hidden := humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", map[string]string{
		"action": "pass", "comment": "hidden alias", "idempotencyKey": "hidden-pass",
	})
	require.Equal(t, http.StatusConflict, hidden.Code, hidden.Body.String())
	assert.Equal(t, []string{"ship", "hold"}, firstObligationForNode(snapshot.State, "implement.do").AvailableActions)
	accepted := humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", map[string]string{
		"action": "SHIP", "comment": "ship reviewed", "idempotencyKey": "custom-ship",
	})
	require.Equal(t, http.StatusOK, accepted.Code, accepted.Body.String())

	resolve("implement.test.quality", "green", []string{"green", "red"})
	resolve("implement.review", "merge", []string{"merge", "revise"})
	result := capstoneTick(t, host)
	assert.Equal(t, state.RunStatusCompleted, result.Status)
}

func TestProcessEngineHumanUnroutableChoicesFailLoudly(t *testing.T) {
	_, root := processEngineFlow(t)
	performer := model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Ask: "Ship?",
		Choices: []string{"ship", "hold"}, ChoiceOutcomes: map[string]string{"ship": "pass"},
	}
	createEngineRun(t, root, "unroutable-choice-run", programTemplate("unroutable-choice", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Error, `choice "hold" requires an explicit pass or fail outcome`)
}

func TestProcessEngineHumanObligationResolvesThroughDashboardMessages(t *testing.T) {
	_, root := processEngineFlow(t)
	fs := createEngineRun(t, root, "human-dashboard-run", programTemplate("human-dashboard-process", model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve from Messages?",
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	message := messages[0]
	assert.Equal(t, "human-dashboard-run", message.ProcessRunID)
	assert.NotEmpty(t, message.ProcessCommandID)
	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": message.ID, "body": "approve looks good in dashboard"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	snapshot, err := fs.LoadRun(t.Context(), "human-dashboard-run")
	require.NoError(t, err)
	assert.Equal(t, state.ActorRef("human:operator"), snapshot.State.Nodes["work"].ActiveAttempt.Actor)
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(snapshot.State).Status)
}

func TestProcessEngineHumanDecisionAdvertisesAndPreservesEdgeVerdict(t *testing.T) {
	_, root := processEngineFlow(t)
	performer := model.Performer{Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve the release?"}
	fs := createEngineRun(t, root, "human-decision-run", decisionTemplate("human-decision", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "human-decision-run")
	require.NoError(t, err)
	assert.Equal(t, []string{"approve", "reject"}, firstObligation(snapshot.State).AvailableActions)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": messages[0].ID, "body": "approve release reviewed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	snapshot, err = fs.LoadRun(t.Context(), "human-decision-run")
	require.NoError(t, err)
	assert.Equal(t, "approve", snapshot.State.Nodes["decide"].ChosenEdge)
}

func TestProcessWorklistActionUsesObservationFunnelAndIsIdempotent(t *testing.T) {
	f, root := processEngineFlow(t)
	performer := model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve the release?",
		Contact: &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:oncall"},
	}
	fs := createEngineRun(t, root, "worklist-decision-run", decisionTemplate("worklist-decision", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	corruptDir := filepath.Join(root, "runs", "corrupt-run")
	require.NoError(t, os.MkdirAll(corruptDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(corruptDir, "run.json"), []byte("{not-json"), 0o644))

	rec := processEngineGet(t, f, "/v1/process/worklist?assignee=human:operator&kind=decision-needed&status=pending")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items        []worklist.Item `json:"items"`
		DegradedRuns []struct {
			Run   string `json:"run"`
			Error string `json:"error"`
		} `json:"degradedRuns"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	require.Len(t, listing.DegradedRuns, 1)
	assert.Equal(t, "corrupt-run", listing.DegradedRuns[0].Run)
	assert.NotEmpty(t, listing.DegradedRuns[0].Error)
	item := listing.Items[0]
	require.NotNil(t, item.Nudge)
	assert.Equal(t, 0, item.Nudge.BudgetUsed)
	assert.Equal(t, 5, item.Nudge.BudgetMax)
	assert.Equal(t, "human:oncall", item.Nudge.EscalationTarget)
	assert.False(t, item.Nudge.NextContactAt.IsZero())

	body := map[string]string{"action": "approve", "comment": "release reviewed", "idempotencyKey": "dashboard-submit-1"}
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	afterFirst, err := fs.LoadRun(t.Context(), "worklist-decision-run")
	require.NoError(t, err)
	firstSeq := afterFirst.State.LastLogSeq
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(afterFirst.State).Status)

	// The same submission goes through RecordOutstandingObservation again;
	// its existing observed-command check makes it a true no-op.
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	afterReplay, err := fs.LoadRun(t.Context(), "worklist-decision-run")
	require.NoError(t, err)
	assert.Equal(t, firstSeq, afterReplay.State.LastLogSeq)
	conflicting := map[string]string{"action": "approve", "comment": "different payload", "idempotencyKey": "dashboard-submit-1"}
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", conflicting)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	settled, err := fs.LoadRun(t.Context(), "worklist-decision-run")
	require.NoError(t, err)
	require.Len(t, settled.State.Nodes["decide"].Decisions, 1)
	decision := settled.State.Nodes["decide"].Decisions[0]
	assert.Equal(t, state.ActorRef("human:operator"), decision.Actor)
	assert.Equal(t, "approve", decision.Verdict)
	assert.Contains(t, decision.EvidenceRef, "worklist-action:sha256:")
}

func TestProcessWorklistDecisionActionPreservesAdvertisedCasing(t *testing.T) {
	f, root := processEngineFlow(t)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "capital-decision", Start: "decide",
		Nodes: map[string]model.Node{
			"decide": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve release?"},
				Next:      model.Next{"Approve": "end", "Reject": "failed"},
			},
			"end": {Type: model.NodeTypeEnd}, "failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	fs := createEngineRun(t, root, "capital-decision-run", tmpl, false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	rec := processEngineGet(t, f, "/v1/process/worklist?run=capital-decision-run&status=pending")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	assert.Equal(t, []string{"Approve", "Reject"}, listing.Items[0].AvailableActions)
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+listing.Items[0].ID+"/action", map[string]string{
		"action": "approve", "comment": "capitalized edge reviewed", "idempotencyKey": "capital-1",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "capital-decision-run")
	require.NoError(t, err)
	assert.Equal(t, "Approve", snapshot.State.Nodes["decide"].ChosenEdge)
}

func TestProcessWorklistRejectsAgentObligationActionWithoutEvidence(t *testing.T) {
	f, root := processEngineFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "worklist-agent", Harness: "claude"})
	require.NoError(t, err)
	fs := createEngineRun(t, root, "agent-obligation-run", programTemplate("agent-obligation", model.Performer{
		Kind: model.PerformerAgent, Profile: "worklist-agent", Prompt: "Implement the change",
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	rec := processEngineGet(t, f, "/v1/process/worklist?run=agent-obligation-run&kind=agent-obligation&status=pending")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	snapshot, err := fs.LoadRun(t.Context(), "agent-obligation-run")
	require.NoError(t, err)
	commandID := outstandingCommandForNode(t, snapshot.State, "work", state.CommandKindStartAttempt)
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/process/worklist/"+listing.Items[0].ID+"/action", map[string]string{
		"action": "pass", "comment": "no durable artifact", "idempotencyKey": "agent-action-1",
	})
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(req, agentRow.CurrentConvID))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "report route")
	after, err := fs.LoadRun(t.Context(), "agent-obligation-run")
	require.NoError(t, err)
	assert.Equal(t, state.CommandStatusIssued, after.State.OutstandingCommands[commandID].Status)
}

func TestProcessWorklistBlockedActionUsesUnblockFunnel(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, _ := createCapstoneRun(t, root, "worklist-blocked-run", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "worklist-blocked-run")
	capstoneReportAgent(t, f, fs, "worklist-blocked-run", "implement.do", "commit:block-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "worklist-blocked-run", "implement.do", "commit:block-2")
	capstoneTickWaiting(t, host, "escalate")

	rec := processEngineGet(t, f, "/v1/process/worklist?run=worklist-blocked-run&kind=blocked&status=pending")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	item := listing.Items[0]
	assert.Equal(t, "implement.test.tests", item.Node)
	assert.Contains(t, item.Summary, "exhausted its budget")
	assert.Equal(t, "human:operator", item.Assignee)
	assert.False(t, item.CreatedAt.IsZero())
	assert.Equal(t, item.CreatedAt, item.ChangedAt)
	require.NotNil(t, item.Nudge)
	assert.Equal(t, processexec.DefaultHumanContactBudget, item.Nudge.BudgetMax)
	assert.False(t, item.Nudge.NextContactAt.IsZero())
	blockedSnapshot, err := fs.LoadRun(t.Context(), "worklist-blocked-run")
	require.NoError(t, err)
	assert.Equal(t, item.CreatedAt, blockedSnapshot.State.Nodes["implement.test.tests"].BlockedAt)
	runRec := processEngineGet(t, f, "/v1/process/runs/worklist-blocked-run")
	require.Equal(t, http.StatusOK, runRec.Code, runRec.Body.String())
	var runView struct {
		State *state.State `json:"state"`
	}
	testharness.DecodeJSON(t, runRec, &runView)
	require.NotNil(t, runView.State)
	assert.Equal(t, item.CreatedAt, runView.State.Nodes["implement.test.tests"].BlockedAt,
		"the live viewer payload is the durable state shape")
	var foreignAgentConv string
	for commandID := range blockedSnapshot.State.OutstandingCommands {
		agentRow, lookupErr := db.AgentForProcessCommand(commandID)
		require.NoError(t, lookupErr)
		if agentRow != nil {
			foreignAgentConv = agentRow.CurrentConvID
			break
		}
	}
	require.NotEmpty(t, foreignAgentConv)
	foreignReq := testharness.JSONRequest(t, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", map[string]string{
		"action": "retry", "comment": "agent must not resolve human work", "idempotencyKey": "foreign-agent-1",
	})
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(foreignReq, foreignAgentConv))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	body := map[string]string{"action": "retry", "comment": "transient failure reviewed", "idempotencyKey": "blocked-submit-1"}
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resolved, err := fs.LoadRun(t.Context(), "worklist-blocked-run")
	require.NoError(t, err)
	assert.Equal(t, 1, blockResolutionCount(resolved.State))
	require.NotNil(t, resolved.State.Nodes["implement.test.tests"].BlockResolution)
	assert.Equal(t, state.BlockDecisionRetry, resolved.State.Nodes["implement.test.tests"].BlockResolution.Decision)
	assert.Equal(t, state.ActorRef("human:operator"), resolved.State.Nodes["implement.test.tests"].BlockResolution.Actor)
	for commandID, command := range resolved.State.OutstandingCommands {
		if command.Kind != state.CommandKindBlockNode || command.NodeID != "implement.test.tests" {
			continue
		}
		contact := resolved.State.Contacts[commandID]
		assert.True(t, contact.Paused)
		assert.Equal(t, "block resolved", contact.PauseReason)
		assert.True(t, contact.NextContactAt.IsZero())
	}

	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	replayed, err := fs.LoadRun(t.Context(), "worklist-blocked-run")
	require.NoError(t, err)
	assert.Equal(t, 1, blockResolutionCount(replayed.State), "idempotent replay appended another resolution audit")
}

func TestProcessEngineNudgeBudgetEscalatesAndResetsOnRecovery(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "nudge-run", programTemplate("nudge", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 1, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:nudges", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 0, adapter.escalations)

	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 1, adapter.escalations)
	snapshot, err := fs.LoadRun(t.Context(), "nudge-run")
	require.NoError(t, err)
	contact := firstContact(snapshot.State)
	assert.False(t, contact.EscalatedAt.IsZero())
	assert.Equal(t, 1, contact.Used)
	assert.Equal(t, state.RunStatusRunning, snapshot.State.Status, "exhaustion escalates and keeps waiting")

	now = now.Add(time.Second)
	adapter.activity = processexec.Activity{Recovered: true, At: now}
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err = fs.LoadRun(t.Context(), "nudge-run")
	require.NoError(t, err)
	contact = firstContact(snapshot.State)
	assert.Zero(t, contact.Used)
	assert.True(t, contact.EscalatedAt.IsZero())
	assert.Equal(t, now.Add(time.Second), contact.NextContactAt)
}

func TestProcessEngineServicesAndStopsBlockedOwnerContact(t *testing.T) {
	_, root := processEngineFlow(t)
	doPerformer := model.Performer{Kind: model.PerformerProgram, Run: "true"}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "blocked-contact", Start: "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask, Performer: &doPerformer,
				Checks: []model.Step{{ID: "tests", Performer: model.Performer{Kind: model.PerformerProgram, Run: "false"}}},
				Next:   model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	fs := createEngineRun(t, root, "blocked-contact-run", tmpl, true)
	adapter := &deferredContactAdapter{}
	host := processengine.New(fs, "agentd:blocked-contact", map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{DefaultTimeout: 5 * time.Second},
		model.PerformerHuman:   adapter,
	})
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	host.Now = func() time.Time { return now }
	host.Executor.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	snapshot, err := fs.LoadRun(t.Context(), "blocked-contact-run")
	require.NoError(t, err)
	blocked := snapshot.State.Nodes["work.test.tests"]
	require.Equal(t, state.NodeStatusBlocked, blocked.Status)
	assert.Equal(t, now, blocked.BlockedAt)
	contact := blockContactForNode(t, snapshot.State, "work.test.tests")
	assert.Equal(t, now.Add(processexec.DefaultHumanContactCadence), contact.NextContactAt)

	now = now.Add(processexec.DefaultHumanContactCadence + time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)

	request, err := processexec.BindBlockResolution(snapshot, processexec.BlockResolutionRequest{
		RunID: "blocked-contact-run", NodeID: "work.test.tests", BlockedAttempt: 1,
		Decision: state.BlockDecisionRetry, Actor: "human:operator", Reason: "transient", EvidenceRef: "decision:block-contact",
	})
	require.NoError(t, err)
	_, err = host.Executor.ResolveBlocked(t.Context(), request)
	require.NoError(t, err)
	resolved, err := fs.LoadRun(t.Context(), "blocked-contact-run")
	require.NoError(t, err)
	contact = blockContactForNode(t, resolved.State, "work.test.tests")
	assert.True(t, contact.Paused)
	assert.Equal(t, "block resolved", contact.PauseReason)
	assert.True(t, contact.NextContactAt.IsZero())

	now = now.Add(time.Hour)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges, "resolved block contact must never fire again")
}

func TestProcessEngineHumanPreemptionPausesAgentAutomation(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "preempt-run", programTemplate("preempt", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 2, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:preempt", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	now = now.Add(10 * time.Second)
	adapter.activity = processexec.Activity{HumanInteracted: true, At: now.Add(-6 * time.Second)}
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Waiting, "automation paused")
	assert.Zero(t, adapter.nudges)
	snapshot, err := fs.LoadRun(t.Context(), "preempt-run")
	require.NoError(t, err)
	assert.True(t, firstContact(snapshot.State).Paused)

	// Real agent activity clears the human-preemption latch and schedules the
	// next contact from a fresh budget.
	now = now.Add(time.Second)
	adapter.activity = processexec.Activity{Recovered: true, At: now}
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err = fs.LoadRun(t.Context(), "preempt-run")
	require.NoError(t, err)
	contact := firstContact(snapshot.State)
	assert.False(t, contact.Paused)
	assert.Empty(t, contact.PauseReason)
	assert.True(t, contact.HumanInteractedAt.IsZero())
}

func TestProcessEngineAutomatedDeliveryDoesNotPauseAgent(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "automated-delivery-run", programTemplate("automated-delivery", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 2, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 1, 30, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:automated-delivery", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	now = now.Add(10 * time.Second)
	adapter.activity = processexec.Activity{AutomatedDelivery: true, At: now.Add(-6 * time.Second)}
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "automated-delivery-run")
	require.NoError(t, err)
	assert.False(t, firstContact(snapshot.State).Paused)
	assert.Equal(t, 1, adapter.nudges)
}

type deferredContactAdapter struct {
	nudges      int
	escalations int
	activity    processexec.Activity
}

func (*deferredContactAdapter) Validate(processexec.Request) error { return nil }
func (*deferredContactAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, errors.New("unexpected synchronous perform")
}
func (*deferredContactAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{ExternalRef: "agent:agt_fake", Assignee: "agent:agt_fake", Summary: "fake work", CreateObligation: true}, nil
}
func (*deferredContactAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}
func (a *deferredContactAdapter) Contact(_ context.Context, _ processexec.Request, escalation bool) error {
	if escalation {
		a.escalations++
	} else {
		a.nudges++
	}
	return nil
}
func (a *deferredContactAdapter) Activity(context.Context, processexec.Request, time.Time) (processexec.Activity, error) {
	return a.activity, nil
}

func firstContact(st *state.State) state.ContactState {
	for _, contact := range st.Contacts {
		return contact
	}
	return state.ContactState{}
}

func blockContactForNode(t *testing.T, st *state.State, nodeID string) state.ContactState {
	t.Helper()
	for commandID, command := range st.OutstandingCommands {
		if command.Kind == state.CommandKindBlockNode && command.NodeID == nodeID {
			contact, ok := st.Contacts[commandID]
			require.True(t, ok, "block command %s has no contact", commandID)
			return contact
		}
	}
	t.Fatalf("node %s has no block command", nodeID)
	return state.ContactState{}
}

func firstObligation(st *state.State) state.ObligationRecord {
	for _, obligation := range st.Obligations {
		return obligation
	}
	return state.ObligationRecord{}
}

func firstObligationForNode(st *state.State, nodeID string) state.ObligationRecord {
	for _, obligation := range st.Obligations {
		if obligation.NodeID == nodeID && obligation.Status == state.WaitStatusPending {
			return obligation
		}
	}
	return state.ObligationRecord{}
}

func TestProcessEngineLeaseContentionAllowsOnlyOneScheduler(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := newBlockingAdapter()
	fs := createEngineRun(t, root, "lease-run", programTemplate("lease", model.Performer{Kind: model.PerformerProgram, Run: "/fake"}), true)
	baseTime := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	controlledNow := baseTime
	setNow := func(next time.Time) {
		clockMu.Lock()
		controlledNow = next
		clockMu.Unlock()
	}
	_ = fs.SetNowForTest(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return controlledNow
	})
	observedStore := newHeartbeatObservingStore(fs, "agentd:first")
	first := processengine.New(observedStore, "agentd:first", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.LeaseTTL = 150 * time.Millisecond
	heartbeatStarted := make(chan time.Duration, 1)
	heartbeatTicks := make(chan time.Time)
	_ = first.SetHeartbeatTimerForTest(func(interval time.Duration) (<-chan time.Time, func()) {
		heartbeatStarted <- interval
		return heartbeatTicks, func() {}
	})
	second := processengine.New(observedStore, "agentd:second", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	second.LeaseTTL = 150 * time.Millisecond
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(adapter.release) }) }

	firstDone := make(chan struct{})
	var firstResults []processengine.RunResult
	secondDone := make(chan struct{})
	var secondResults []processengine.RunResult
	var secondErr error
	secondStarted := false
	go func() {
		firstResults, _ = agentd.RunProcessEngineTickForTest(t.Context(), first)
		close(firstDone)
	}()
	t.Cleanup(func() {
		releaseFirst()
		select {
		case <-firstDone:
		case <-time.After(2 * time.Second):
			t.Error("first scheduler did not stop during cleanup")
		}
		if secondStarted {
			select {
			case <-secondDone:
			case <-time.After(2 * time.Second):
				t.Error("second scheduler did not stop during cleanup")
			}
		}
	})
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler never reached performer")
	}
	select {
	case interval := <-heartbeatStarted:
		assert.Equal(t, first.LeaseTTL/3, interval)
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler did not start its heartbeat timer")
	}
	// Renew halfway through the original lease, then move just beyond its
	// original expiry. The second scheduler must still see the renewed lease.
	setNow(baseTime.Add(first.LeaseTTL / 2))
	select {
	case heartbeatTicks <- baseTime.Add(first.LeaseTTL / 2):
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler did not accept the controlled heartbeat tick")
	}
	var renewed store.LeaseRecord
	select {
	case renewed = <-observedStore.renewed:
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler did not renew its lease")
	}
	assert.Equal(t, baseTime.Add(first.LeaseTTL/2), renewed.UpdatedAt)
	assert.Equal(t, baseTime.Add(first.LeaseTTL+first.LeaseTTL/2), renewed.ExpiresAt)
	setNow(baseTime.Add(first.LeaseTTL + time.Nanosecond))
	secondStarted = true
	go func() {
		secondResults, secondErr = agentd.RunProcessEngineTickForTest(t.Context(), second)
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second scheduler did not return; it may have acquired the renewed lease")
	}
	require.NoError(t, secondErr)
	require.Len(t, secondResults, 1)
	assert.True(t, secondResults[0].LeaseContended, "result: %+v", secondResults[0])
	assert.Contains(t, secondResults[0].Error, store.ErrLeaseHeld.Error())
	releaseFirst()
	select {
	case <-firstDone:
		require.Len(t, firstResults, 1)
		assert.Empty(t, firstResults[0].Error)
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
	manifestPath := filepath.Join(root, "runs", "dirty-run", "manifest.jsonl")
	body, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	require.NotEmpty(t, lines)
	var entry evidence.ManifestEntry
	require.NoError(t, json.Unmarshal(lines[0], &entry))
	entry.EntryChecksum = "deliberately-corrupted"
	lines[0], err = json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, append(bytes.Join(lines, []byte("\n")), '\n'), 0o644))

	host := processengine.New(fs, "agentd:dirty", map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
	})
	var firstReason string
	for tick := 0; tick < 2; tick++ {
		results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, state.RunStatusInconsistent, results[0].Status)
		assert.Contains(t, results[0].Waiting, "checksum")
		if tick == 0 {
			firstReason = results[0].Waiting
		} else {
			assert.Equal(t, firstReason, results[0].Waiting, "inconsistent run must stay halted across ticks")
		}
	}
	_, err = os.Stat(output)
	assert.ErrorIs(t, err, os.ErrNotExist)

	rec := processEngineGet(t, f, "/v1/process/runs/dirty-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"effectiveStatus":"inconsistent"`)
	assert.Contains(t, rec.Body.String(), "checksum")
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
	return createEngineRunWithParams(t, root, runID, tmpl, allowPrograms, nil)
}

func createEngineRunWithParams(t *testing.T, root, runID string, tmpl *model.Template, allowPrograms bool, params map[string]string) *store.FS {
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
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: templateRecord.Ref, Params: params}, initial)
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

func createCapstoneRun(t *testing.T, root, runID string, programPassAt int) (*store.FS, string) {
	t.Helper()
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "dev", Harness: "claude"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)

	source, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "examples", "code-change-with-review.yaml"))
	require.NoError(t, err)
	parsed, err := model.Parse(source)
	require.NoError(t, err)
	require.False(t, parsed.Diagnostics.HasErrors(), "template diagnostics: %#v", parsed.Diagnostics.Errors())
	tmpl := parsed.Template
	implement := tmpl.Nodes["implement"]
	require.Len(t, implement.Checks, 2)
	marker := filepath.Join(t.TempDir(), runID+"-program-count")
	implement.Checks[0].Performer.Run = "/bin/sh"
	implement.Checks[0].Performer.Args = []string{
		"-c",
		`count=0; if [ -f "$1" ]; then read -r count < "$1"; fi; count=$((count + 1)); printf '%s\n' "$count" > "$1"; [ "$count" -ge "$2" ]`,
		"process-capstone", marker, strconv.Itoa(programPassAt),
	}
	tmpl.Nodes["implement"] = implement
	return createEngineRunWithParams(t, root, runID, tmpl, true, map[string]string{"issue": "TCL-278"}), marker
}

func capstoneReachDo(t *testing.T, f *testharness.Flow, fs *store.FS, host *processengine.Host, runID string) {
	t.Helper()
	capstoneTickWaiting(t, host, "implement.plan")
	capstoneReportAgent(t, f, fs, runID, "implement.plan", "artifact:plan")
	capstoneTickWaiting(t, host, "implement.plan.approval")
	capstoneReplyHuman(t, fs, runID, "implement.plan.approval", "approve plan reviewed")
	capstoneTickWaiting(t, host, "implement.do")
}

func capstoneTick(t *testing.T, host *processengine.Host) processengine.RunResult {
	t.Helper()
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)
	return results[0]
}

func capstoneTickWaiting(t *testing.T, host *processengine.Host, nodeID string) processengine.RunResult {
	t.Helper()
	result := capstoneTick(t, host)
	snapshot, err := host.Store.LoadRun(t.Context(), result.RunID)
	require.NoError(t, err)
	commandID := outstandingCommandForNode(t, snapshot.State, nodeID, "")
	assert.Equal(t, state.CommandStatusIssued, snapshot.State.OutstandingCommands[commandID].Status)
	return result
}

func outstandingCommandForNode(t *testing.T, st *state.State, nodeID string, kind state.CommandKind) string {
	t.Helper()
	for commandID, command := range st.OutstandingCommands {
		if command.NodeID == nodeID && command.Status == state.CommandStatusIssued && (kind == "" || command.Kind == kind) {
			return commandID
		}
	}
	t.Fatalf("no issued %s command for node %s", kind, nodeID)
	return ""
}

func capstoneReportAgent(t *testing.T, f *testharness.Flow, fs *store.FS, runID, nodeID, evidenceRef string) {
	t.Helper()
	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	commandID := outstandingCommandForNode(t, snapshot.State, nodeID, state.CommandKindStartAttempt)
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	brief, ok := f.World.SpawnInitialPrompt(agentRow.CurrentConvID)
	require.True(t, ok)
	assert.Contains(t, brief, "TCL-278")
	assert.NotContains(t, brief, "{{ params.issue }}")
	body := map[string]string{"command_id": commandID, "verdict": "pass", "evidence_ref": evidenceRef}
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs/"+runID+"/nodes/"+nodeID+"/report", body)
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, agentRow.CurrentConvID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func capstoneReplyHuman(t *testing.T, fs *store.FS, runID, nodeID, reply string) {
	t.Helper()
	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	commandID := ""
	for id, command := range snapshot.State.OutstandingCommands {
		if command.NodeID == nodeID && command.Status == state.CommandStatusIssued &&
			(command.Kind == state.CommandKindRecordDecision || command.Kind == state.CommandKindStartAttempt) {
			commandID = id
			break
		}
	}
	require.NotEmpty(t, commandID, "no issued human command for %s", nodeID)
	message, err := db.FindHumanMessageForProcessCommand(commandID, "Process obligation")
	require.NoError(t, err)
	require.NotNil(t, message)
	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": message.ID, "body": reply})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func blockResolutionCount(st *state.State) int {
	count := 0
	for _, record := range st.AdminRecords {
		if record.Type == state.EventBlockResolutionRecorded {
			count++
		}
	}
	return count
}

func assertCapstoneAuditableFromRunDir(t *testing.T, root, runID string) {
	t.Helper()
	runDir := filepath.Join(root, "runs", runID)
	for _, relative := range []string{"run.json", "state.json", "manifest.jsonl", filepath.Join("run", "log.jsonl")} {
		info, err := os.Stat(filepath.Join(runDir, relative))
		require.NoError(t, err, relative)
		assert.Positive(t, info.Size(), relative)
	}
	nodeLogs, err := filepath.Glob(filepath.Join(runDir, "nodes", "*", "log.jsonl"))
	require.NoError(t, err)
	assert.NotEmpty(t, nodeLogs)
	artifacts, err := os.ReadDir(filepath.Join(runDir, "artifacts"))
	require.NoError(t, err)
	assert.NotEmpty(t, artifacts)

	// Remove the store-level template library before reconstruction. A fresh
	// store can still verify from the template snapshot pinned in run.json plus
	// the state/log/manifest/artifact files under this run directory.
	templateArchive := filepath.Join(t.TempDir(), "templates")
	require.NoError(t, os.Rename(filepath.Join(root, "templates"), templateArchive))
	fresh, err := store.NewFS(root)
	require.NoError(t, err)
	report := processverify.StoreRun(t.Context(), fresh, runID)
	assert.False(t, report.HasErrors(), "run-dir verification: %#v", report.Diagnostics)
}

type cancelAfterResolveClaimStore struct {
	store.Store
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelAfterResolveClaimStore) Append(ctx context.Context, runID string, expectedSeq int64, entries []evidence.LogEntry) (store.AppendResult, error) {
	result, err := s.Store.Append(ctx, runID, expectedSeq, entries)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if entry.Event != nil && entry.Event.Type == state.EventCommandIssued && entry.Event.Command != nil && entry.Event.Command.Kind == state.CommandKindResolveBlock {
			s.once.Do(s.cancel)
		}
	}
	return result, nil
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

func decisionTemplate(id string, performer model.Performer) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         id,
		Start:      "decide",
		Nodes: map[string]model.Node{
			"decide": {Type: model.NodeTypeDecision, Performer: &performer, Next: model.Next{"approve": "end", "reject": "failed"}},
			"end":    {Type: model.NodeTypeEnd},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
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

type captureInstantiateAdapter struct {
	mu       sync.Mutex
	captured processexec.Request
}

func (a *captureInstantiateAdapter) Validate(processexec.Request) error { return nil }

func (a *captureInstantiateAdapter) Perform(_ context.Context, request processexec.Request) (processexec.Observation, error) {
	a.mu.Lock()
	a.captured = request
	a.mu.Unlock()
	return processexec.Observation{Actor: "agent:agt_instantiate", Verdict: "pass", EvidenceRef: "artifact:instantiate-flow"}, nil
}

func (a *captureInstantiateAdapter) request() processexec.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.captured
}

type countingProcessStore struct {
	store.Store
	mu     sync.Mutex
	loads  int
	leases int
}

type heartbeatObservingStore struct {
	store.Store
	holder  string
	renewed chan store.LeaseRecord
	mu      sync.Mutex
	claims  int
}

func newHeartbeatObservingStore(st store.Store, holder string) *heartbeatObservingStore {
	return &heartbeatObservingStore{Store: st, holder: holder, renewed: make(chan store.LeaseRecord, 1)}
}

func (s *heartbeatObservingStore) AcquireRunLease(ctx context.Context, runID, holder string, ttl time.Duration) (store.LeaseRecord, error) {
	lease, err := s.Store.AcquireRunLease(ctx, runID, holder, ttl)
	if err != nil || holder != s.holder {
		return lease, err
	}
	s.mu.Lock()
	s.claims++
	isRenewal := s.claims == 2
	s.mu.Unlock()
	if isRenewal {
		s.renewed <- lease
	}
	return lease, nil
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
