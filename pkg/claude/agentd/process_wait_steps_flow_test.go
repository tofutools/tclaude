package agentd_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_96aee1ac: wait steps run. A run waits at a wait step until its deadline
// (a duration after the step starts, an until time, or the earlier of the
// two), then goes on. These flows drive real runs through the daemon mux on
// an injected clock, and read the run view and its evidence.

// waitClock is the fake clock the wait sweep reads.
type waitClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *waitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *waitClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type waitRunView struct {
	ID       string              `json:"id"`
	Status   engine.RunStatus    `json:"status"`
	Action   string              `json:"action"`
	Failure  *engine.StepFailure `json:"failure"`
	Commands []struct {
		NodeID string             `json:"nodeId"`
		Kind   engine.CommandKind `json:"kind"`
		State  string             `json:"state"`
		Wait   *struct {
			Duration  string     `json:"duration"`
			Until     string     `json:"until"`
			StartedAt *time.Time `json:"startedAt"`
			DueAt     *time.Time `json:"dueAt"`
		} `json:"wait"`
	} `json:"commands"`
}

// waitFlow starts a run of start → hold (wait) → end, with params, on a
// fake clock, and has the daemon fix the wait's deadline.
func waitFlow(t *testing.T, runID string, wait model.WaitConfig, params map[string]string) (*testharness.Flow, *waitClock) {
	t.Helper()
	f, root := processRuntimeFlow(t)
	clock := &waitClock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	t.Cleanup(agentd.SetProcessWaitNowForTest(clock.Now))
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: runID + "-tmpl", Start: "start",
		Params: map[string]model.Param{"at": {Type: "string"}},
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "hold"}},
			"hold":  {Type: model.NodeTypeWait, Wait: &wait, Next: model.Next{"next": "end"}},
			"end":   {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "params": params, "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.SweepProcessWaitsForTest()
	return f, clock
}

func showWaitRun(t *testing.T, f *testharness.Flow, runID string) waitRunView {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view waitRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

// waitEvidence is the run's wait_finished events' payloads.
func waitEvidence(t *testing.T, f *testharness.Flow, runID string) []map[string]any {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID+"/events", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Events []struct {
			Kind    string         `json:"kind"`
			Actor   string         `json:"actor"`
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	testharness.DecodeJSON(t, rec, &body)
	var out []map[string]any
	for _, event := range body.Events {
		if event.Kind == "wait_finished" {
			event.Payload["actor"] = event.Actor
			out = append(out, event.Payload)
		}
	}
	return out
}

// Scenario: a run waits 2 hours at a wait step, showing when it is due, and
// goes on once the time has come. Sweeping again later never moves the
// deadline, which is what keeps it across a daemon restart.
func TestProcessWait_EndsWhenItsTimeComes(t *testing.T) {
	f, clock := waitFlow(t, "run_wait_2h", model.WaitConfig{Duration: "2h"}, nil)
	start := clock.Now()
	view := showWaitRun(t, f, "run_wait_2h")
	require.Equal(t, engine.RunRunning, view.Status)
	assert.Equal(t, "waiting", view.Action)
	require.Len(t, view.Commands, 1)
	hold := view.Commands[0]
	assert.Equal(t, engine.CommandWait, hold.Kind)
	assert.Equal(t, "waiting", hold.State)
	require.NotNil(t, hold.Wait)
	require.NotNil(t, hold.Wait.DueAt)
	assert.Equal(t, "2h", hold.Wait.Duration)
	assert.True(t, hold.Wait.DueAt.Equal(start.Add(2*time.Hour)), "due %v", hold.Wait.DueAt)

	clock.Advance(time.Hour)
	agentd.SweepProcessWaitsForTest()
	view = showWaitRun(t, f, "run_wait_2h")
	require.Equal(t, engine.RunRunning, view.Status, "an hour in, it still waits")
	assert.True(t, view.Commands[0].Wait.DueAt.Equal(start.Add(2*time.Hour)), "the deadline is fixed once")

	clock.Advance(time.Hour)
	agentd.SweepProcessWaitsForTest()
	view = showWaitRun(t, f, "run_wait_2h")
	assert.Equal(t, engine.RunCompleted, view.Status)
	evidence := waitEvidence(t, f, "run_wait_2h")
	require.Len(t, evidence, 1)
	assert.Equal(t, "elapsed", evidence[0]["how"])
	assert.Equal(t, "system:process-runtime", evidence[0]["actor"])
}

// Scenario: "until a param's time, but at most 3 days": the earlier wins.
func TestProcessWait_UntilAParamTimeAtMostADuration(t *testing.T) {
	at := time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC).Format(time.RFC3339)
	f, clock := waitFlow(t, "run_wait_until", model.WaitConfig{Duration: "3 days", Until: "{{ params.at }}"},
		map[string]string{"at": at})
	view := showWaitRun(t, f, "run_wait_until")
	require.NotNil(t, view.Commands[0].Wait.DueAt)
	assert.True(t, view.Commands[0].Wait.DueAt.Equal(clock.Now().Add(30*time.Minute)), "due %v", view.Commands[0].Wait.DueAt)
	assert.Equal(t, at, view.Commands[0].Wait.Until, "the param is bound into the step")
	clock.Advance(30 * time.Minute)
	agentd.SweepProcessWaitsForTest()
	assert.Equal(t, engine.RunCompleted, showWaitRun(t, f, "run_wait_until").Status)
}

// Scenario: the wall clock keeps running while a run is paused. A wait whose
// time came while paused ends once the run is resumed, not before.
func TestProcessWait_APausedRunEndsItsWaitOnResume(t *testing.T) {
	f, clock := waitFlow(t, "run_wait_pause", model.WaitConfig{Duration: "1h"}, nil)
	paused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_wait_pause/pause", map[string]any{})
	require.Equal(t, http.StatusOK, paused.Code, paused.Body.String())
	clock.Advance(2 * time.Hour)
	agentd.SweepProcessWaitsForTest()
	require.Equal(t, engine.RunRunning, showWaitRun(t, f, "run_wait_pause").Status, "a paused run's wait does not end")
	resumed := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_wait_pause/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, resumed.Code, resumed.Body.String())
	agentd.SweepProcessWaitsForTest()
	assert.Equal(t, engine.RunCompleted, showWaitRun(t, f, "run_wait_pause").Status)
}

// Scenario: someone with process.runs.manage skips a wait; the run goes on
// at once, and the evidence says who skipped it. A step that is not waiting
// cannot be skipped.
func TestProcessWait_SkipEndsItNow(t *testing.T) {
	f, _ := waitFlow(t, "run_wait_skip", model.WaitConfig{Duration: "3 days"}, nil)
	wrong := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_wait_skip/skip-wait", map[string]any{"nodeId": "end"})
	require.Equal(t, http.StatusConflict, wrong.Code, wrong.Body.String())
	skipped := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_wait_skip/skip-wait", map[string]any{"nodeId": "hold"})
	require.Equal(t, http.StatusAccepted, skipped.Code, skipped.Body.String())
	assert.Equal(t, engine.RunCompleted, showWaitRun(t, f, "run_wait_skip").Status)
	evidence := waitEvidence(t, f, "run_wait_skip")
	require.Len(t, evidence, 1)
	assert.Equal(t, "skipped", evidence[0]["how"])
	assert.NotEqual(t, "system:process-runtime", evidence[0]["actor"])
	again := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_wait_skip/skip-wait", map[string]any{"nodeId": "hold"})
	assert.Equal(t, http.StatusConflict, again.Code, "an ended wait cannot be skipped again")
}

// Scenario: a wait until an output no step reported cannot work out its
// time: the step fails and so does the run, naming the step and why.
func TestProcessWait_ADeadlineThatIsNotATimeFailsTheRun(t *testing.T) {
	f, _ := waitFlow(t, "run_wait_bad", model.WaitConfig{Until: "{{ outputs.when }}"}, nil)
	view := showWaitRun(t, f, "run_wait_bad")
	require.Equal(t, engine.RunFailed, view.Status)
	require.NotNil(t, view.Failure)
	assert.Equal(t, "hold", view.Failure.NodeID)
	assert.True(t, strings.Contains(view.Failure.Reason, "wait 'hold'") && strings.Contains(view.Failure.Reason, `no step has reported output "when"`),
		view.Failure.Reason)
}

// Scenario: canceling a waiting run withdraws the wait and the run ends
// canceled; a later sweep does not touch it.
func TestProcessWait_CancelWithdrawsTheWait(t *testing.T) {
	f, clock := waitFlow(t, "run_wait_cancel", model.WaitConfig{Duration: "1h"}, nil)
	canceled := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_wait_cancel/cancel", map[string]any{"note": "not needed"})
	require.Equal(t, http.StatusAccepted, canceled.Code, canceled.Body.String())
	view := showWaitRun(t, f, "run_wait_cancel")
	require.Equal(t, engine.RunCanceled, view.Status)
	assert.Empty(t, view.Commands)
	clock.Advance(2 * time.Hour)
	agentd.SweepProcessWaitsForTest()
	assert.Equal(t, engine.RunCanceled, showWaitRun(t, f, "run_wait_cancel").Status)
	evidence := waitEvidence(t, f, "run_wait_cancel")
	require.Len(t, evidence, 1)
	assert.Equal(t, "withdrawn", evidence[0]["how"])
}

// Scenario: an agent step reports a time as an output, and the wait after it
// waits until that time.
func TestProcessWait_UntilATimeAnEarlierStepReported(t *testing.T) {
	f, root := processRuntimeFlow(t)
	clock := &waitClock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	t.Cleanup(agentd.SetProcessWaitNowForTest(clock.Now))
	g := f.HaveGroup("crew")
	const conv = "plan-1111-2222-3333-4444"
	havePerformerAgent(t, f, "crew", conv, "alice", "dev", "idle")
	tmpl := performerTemplate("wait-output", devPerformer(), nil)
	work := tmpl.Nodes[performerNode]
	work.Captures = []string{"when"}
	work.Next = model.Next{"next": "hold"}
	tmpl.Nodes[performerNode] = work
	tmpl.Nodes["hold"] = model.Node{Type: model.NodeTypeWait, Wait: &model.WaitConfig{Until: "{{ outputs.when }}"},
		Next: model.Next{"next": "end"}}
	startPerformerRun(t, f, root, "run_wait_output", tmpl, agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	when := clock.Now().Add(45 * time.Minute)
	rec := reportAs(t, f, conv, map[string]any{"runId": "run_wait_output", "nodeId": performerNode, "attempt": 1,
		"outcome": "succeeded", "outputs": map[string]any{"when": " " + when.Format(time.RFC3339) + "\n"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.SweepProcessWaitsForTest()
	view := showWaitRun(t, f, "run_wait_output")
	require.Equal(t, engine.RunRunning, view.Status, "%+v", view.Failure)
	require.Len(t, view.Commands, 1)
	require.NotNil(t, view.Commands[0].Wait.DueAt)
	assert.True(t, view.Commands[0].Wait.DueAt.Equal(when), "due %v", view.Commands[0].Wait.DueAt)
	clock.Advance(45 * time.Minute)
	agentd.SweepProcessWaitsForTest()
	assert.Equal(t, engine.RunCompleted, showWaitRun(t, f, "run_wait_output").Status)
}

// Scenario: a run was given up but its owner never withdrew the wait (the
// daemon stopped in between). The sweep withdraws it, so the run finishes
// canceled instead of waiting out its 30 days.
func TestProcessWait_AGivenUpRunsWaitIsWithdrawnBySweep(t *testing.T) {
	f, clock := waitFlow(t, "run_wait_doomed", model.WaitConfig{Duration: "30 days"}, nil)
	agentd.MarkProcessRunCanceledForTest(t, "run_wait_doomed")
	agentd.SweepProcessWaitsForTest()
	view := showWaitRun(t, f, "run_wait_doomed")
	assert.Equal(t, engine.RunCanceled, view.Status)
	evidence := waitEvidence(t, f, "run_wait_doomed")
	require.Len(t, evidence, 1)
	assert.Equal(t, "withdrawn", evidence[0]["how"])
	clock.Advance(31 * 24 * time.Hour)
	agentd.SweepProcessWaitsForTest()
	assert.Len(t, waitEvidence(t, f, "run_wait_doomed"), 1, "nothing more happens at its old deadline")
}
