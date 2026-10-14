package agentd_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-zb2iev: an operator pause holds a run. A paused run dispatches no new
// agent turns or program commands; attempts already in flight finish and are
// recorded; resume releases it and planning picks up where it stopped.

type pauseWire struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

type pauseRunView struct {
	ID         string            `json:"id"`
	Status     engine.RunStatus  `json:"status"`
	Action     string            `json:"action"`
	Paused     *pauseWire        `json:"paused"`
	Checkpoint engine.Checkpoint `json:"checkpoint"`
}

type pauseResponse struct {
	Changed  bool         `json:"changed"`
	Started  bool         `json:"started"`
	Unpaused bool         `json:"unpaused"`
	Run      pauseRunView `json:"run"`
}

func pauseRun(t *testing.T, mux http.Handler, req func(method, path string) *http.Request, runID, verb string, want int) pauseResponse {
	t.Helper()
	rec := testharness.Serve(mux, req(http.MethodPost, "/v1/process/runs/"+runID+"/"+verb))
	require.Equal(t, want, rec.Code, rec.Body.String())
	var response pauseResponse
	testharness.DecodeJSON(t, rec, &response)
	return response
}

func pauseHumanReq(t *testing.T) func(method, path string) *http.Request {
	return func(method, path string) *http.Request {
		return agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, map[string]any{}))
	}
}

func pauseDashReq(t *testing.T) func(method, path string) *http.Request {
	return func(method, path string) *http.Request { return dashReq(t, method, path, map[string]any{}) }
}

// Scenario: a sequential program run is paused while its first program is
// executing. That program finishes and its outcome is recorded, but the next
// one is not planned — not by the owner, and not by the fallback sweep. A
// repeated pause changes nothing. Resume releases the run and it completes.
func TestProcessRunPause_InFlightProgramFinishesNothingNewIsPlanned(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("pause-prog", 2))

	var mu sync.Mutex
	var started []string
	running := make(chan struct{}, 2)
	release := make(chan struct{})
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		mu.Lock()
		started = append(started, dispatch.Command().NodeID)
		mu.Unlock()
		running <- struct{}{}
		<-release
		return executor.Perform(ctx, dispatch)
	}))
	startedNodes := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(started)
	}

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_pause_prog", "templateId": "pause-prog", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	<-running

	paused := pauseRun(t, f.Mux, pauseHumanReq(t), "run_pause_prog", "pause", http.StatusOK)
	assert.True(t, paused.Changed)
	require.NotNil(t, paused.Run.Paused)
	assert.Equal(t, "human:operator", paused.Run.Paused.By)
	again := pauseRun(t, f.Mux, pauseHumanReq(t), "run_pause_prog", "pause", http.StatusOK)
	assert.False(t, again.Changed, "pausing a paused run changes nothing")

	close(release)
	agentd.WaitForProcessRunRuntimeForTest()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()

	assert.Equal(t, []string{"task-01"}, startedNodes(), "the in-flight program finished; nothing new started")
	held := showPauseRun(t, f, "run_pause_prog")
	assert.Equal(t, engine.RunRunning, held.Status)
	assert.Equal(t, "paused", held.Action)
	assert.Equal(t, engine.NodeDone, held.Checkpoint.Nodes["task-01"], "the in-flight outcome is recorded")
	assert.Equal(t, engine.NodeReady, held.Checkpoint.Nodes["task-02"])
	assert.Empty(t, held.Checkpoint.Commands, "the successor was not planned")

	resumed := pauseRun(t, f.Mux, pauseHumanReq(t), "run_pause_prog", "resume", http.StatusAccepted)
	assert.True(t, resumed.Unpaused)
	assert.True(t, resumed.Started)
	<-running
	agentd.WaitForProcessRunRuntimeForTest()

	assert.Equal(t, []string{"task-01", "task-02"}, startedNodes())
	done := showPauseRun(t, f, "run_pause_prog")
	assert.Equal(t, engine.RunCompleted, done.Status)
	assert.Nil(t, done.Paused)

	kinds, events := performerEventKinds(t, f, "run_pause_prog")
	assert.Equal(t, 1, countKind(kinds, "run_paused"))
	assert.Equal(t, 1, countKind(kinds, "run_resumed"))
	for _, event := range events {
		if event.Kind == "run_paused" || event.Kind == "run_resumed" {
			assert.Equal(t, "human:operator", event.Actor)
		}
	}
	assert.Less(t, slices.Index(kinds, "run_paused"), slices.Index(kinds, "run_resumed"))

	// A finished run cannot be paused.
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_pause_prog/pause", map[string]any{})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "process_run_pause")
}

func showPauseRun(t *testing.T, f *testharness.Flow, runID string) pauseRunView {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view pauseRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

// epicContextWire is the slice of the /v1/process/context read model the
// end-to-end flow asserts on.
type epicContextWire struct {
	Runs   map[string]epicProjectionWire `json:"runs"`
	Groups []struct {
		Group    string   `json:"group"`
		Active   []string `json:"active"`
		NeedsYou []string `json:"needsYou"`
		Done     []string `json:"done"`
	} `json:"groups"`
}

type epicProjectionWire struct {
	Status string     `json:"status"`
	Action string     `json:"action"`
	Paused *pauseWire `json:"paused"`
	Scope  struct {
		Kind  string `json:"kind"`
		Group string `json:"group"`
	} `json:"scope"`
	Nodes []epicNodeWire `json:"nodes"`
}

type epicNodeWire struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Dispatch *struct {
		NodeID     string `json:"nodeId"`
		AgentID    string `json:"agentId"`
		Name       string `json:"name"`
		Attempt    int    `json:"attempt"`
		State      string `json:"state"`
		WaitReason string `json:"waitReason"`
	} `json:"dispatch"`
}

func (p epicProjectionWire) node(t *testing.T, id string) epicNodeWire {
	t.Helper()
	for _, node := range p.Nodes {
		if node.ID == id {
			return node
		}
	}
	require.Failf(t, "node not projected", "node %q", id)
	return epicNodeWire{}
}

// epicContext reads the read model the daemon serves and the one the
// dashboard reads from its own mux, requires them to agree, and returns the
// run's projection plus its group entry.
func epicContext(t *testing.T, f *testharness.Flow, runID string) (epicProjectionWire, epicContextWire) {
	t.Helper()
	agentd.WaitForProcessDispatchForTest()
	daemon := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, daemon.Code, daemon.Body.String())
	var view epicContextWire
	testharness.DecodeJSON(t, daemon, &view)
	dash := testharness.Serve(agentd.BuildDashboardHandlerForTest(), dashReq(t, http.MethodGet, "/v1/process/context", nil))
	require.Equal(t, http.StatusOK, dash.Code, dash.Body.String())
	var dashboard epicContextWire
	testharness.DecodeJSON(t, dash, &dashboard)
	require.Equal(t, view, dashboard, "the dashboard reads the same read model")
	run, ok := view.Runs[runID]
	require.True(t, ok, daemon.Body.String())
	return run, view
}

func inboxHasNode(t *testing.T, conv, node string) bool {
	t.Helper()
	for _, message := range processInbox(t, conv) {
		if strings.Contains(message.Body, "Node:    "+node+"\n") {
			return true
		}
	}
	return false
}

func epicTemplate() *model.Template {
	dev := devPerformer()
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "epic", Start: "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{"next": "work"}},
			"work":   {Type: model.NodeTypeTask, Performer: &dev, Next: model.Next{"next": "review"}},
			"review": {Type: model.NodeTypeTask, Performer: &dev, Next: model.Next{"next": "ship"}},
			"ship":   {Type: model.NodeTypeTask, Performer: &dev, Next: model.Next{"next": "end"}},
			"end":    {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// Scenario (the epic path, tcl-g4qeen): a template with agent performers is
// started as a group-scoped run. Its first node is dispatched to the idle
// member, who reports back; the next node is reassigned from the dashboard to
// another member; the run is paused from the dashboard while that member
// works. The member's report still lands, but the following node is not
// dispatched until the run is resumed, after which it is dispatched and the
// run completes. The /v1/process/context read model — as the daemon and the
// dashboard serve it — is asserted at every step.
func TestProcessEpic_ScopeDispatchReportReassignPauseResume(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("crew")
	const aliceConv, bobConv = "epca-1111-2222-3333-4444", "epcb-1111-2222-3333-4444"
	alice := havePerformerAgent(t, f, "crew", aliceConv, "alice", "dev", "idle")
	bob := havePerformerAgent(t, f, "crew", bobConv, "bob", "dev", "working")
	putProcessRuntimeTemplate(t, root, epicTemplate())
	dash := agentd.BuildDashboardHandlerForTest()

	// Scope + dispatch: a group-scoped run hands "work" to the idle dev.
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_epic", "templateId": "epic", "group": "crew", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	run, view := epicContext(t, f, "run_epic")
	assert.Equal(t, "group", run.Scope.Kind)
	assert.Equal(t, "crew", run.Scope.Group)
	require.Len(t, view.Groups, 1)
	assert.Contains(t, view.Groups[0].Active, "run_epic")
	assert.Nil(t, run.Paused)
	work := run.node(t, "work")
	require.NotNil(t, work.Dispatch)
	assert.Equal(t, alice, work.Dispatch.AgentID)
	assert.Equal(t, db.ProcessDispatchDelivered, work.Dispatch.State)
	assert.Nil(t, run.node(t, "review").Dispatch)
	require.Len(t, processInbox(t, aliceConv), 1)

	// Report back: alice finishes "work"; "review" goes to her too.
	rec := reportAs(t, f, aliceConv, map[string]any{
		"runId": "run_epic", "nodeId": "work", "attempt": 1, "outcome": "succeeded", "note": "done",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	run, _ = epicContext(t, f, "run_epic")
	assert.Equal(t, string(engine.NodeDone), run.node(t, "work").State)
	review := run.node(t, "review")
	require.NotNil(t, review.Dispatch)
	assert.Equal(t, alice, review.Dispatch.AgentID)

	// Reassign from the dashboard: "review" moves to bob.
	rec = testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/v1/process/runs/run_epic/nodes/"+review.Dispatch.NodeID+"/assign", map[string]any{"agent": "bob"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	run, _ = epicContext(t, f, "run_epic")
	review = run.node(t, "review")
	require.NotNil(t, review.Dispatch)
	assert.Equal(t, bob, review.Dispatch.AgentID)
	assert.Equal(t, "bob", review.Dispatch.Name)
	assert.Equal(t, db.ProcessDispatchDelivered, review.Dispatch.State)
	require.Len(t, processInbox(t, bobConv), 1)

	// Pause from the dashboard while bob works.
	paused := pauseRun(t, dash, pauseDashReq(t), "run_epic", "pause", http.StatusOK)
	assert.True(t, paused.Changed)
	run, view = epicContext(t, f, "run_epic")
	require.NotNil(t, run.Paused)
	assert.Equal(t, "paused", run.Action)
	assert.Contains(t, view.Groups[0].Active, "run_epic", "a paused run is still active, not needs-you")
	rec = testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/v1/process/runs/run_epic/nodes/review/assign", map[string]any{"agent": "alice"}))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "reassigning would start a new turn: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "paused")

	// bob's in-flight report is recorded; "ship" is not dispatched.
	rec = reportAs(t, f, bobConv, map[string]any{
		"runId": "run_epic", "nodeId": "review", "attempt": 1, "outcome": "succeeded", "note": "lgtm",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.RunProcessRunSweepForTest()
	agentd.SweepProcessAgentDispatchesForTest()
	run, _ = epicContext(t, f, "run_epic")
	assert.Equal(t, string(engine.NodeDone), run.node(t, "review").State)
	ship := run.node(t, "ship")
	assert.Equal(t, string(engine.NodeReady), ship.State)
	assert.Nil(t, ship.Dispatch, "a paused run dispatches no new agent turn")
	assert.False(t, inboxHasNode(t, aliceConv, "ship"), "a paused run dispatches no new agent turn")
	assert.False(t, inboxHasNode(t, bobConv, "ship"))

	// Resume from the dashboard: "ship" is dispatched and the run completes.
	resumed := pauseRun(t, dash, pauseDashReq(t), "run_epic", "resume", http.StatusAccepted)
	assert.True(t, resumed.Unpaused)
	run, _ = epicContext(t, f, "run_epic")
	assert.Nil(t, run.Paused)
	ship = run.node(t, "ship")
	require.NotNil(t, ship.Dispatch, "resume dispatches the held node")
	assert.Equal(t, alice, ship.Dispatch.AgentID)
	assert.Equal(t, db.ProcessDispatchDelivered, ship.Dispatch.State)
	assert.True(t, inboxHasNode(t, aliceConv, "ship"))

	rec = reportAs(t, f, aliceConv, map[string]any{
		"runId": "run_epic", "nodeId": "ship", "attempt": 1, "outcome": "succeeded", "note": "shipped",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	run, view = epicContext(t, f, "run_epic")
	assert.Equal(t, string(engine.RunCompleted), run.Status)
	assert.Contains(t, view.Groups[0].Done, "run_epic")

	kinds, _ := performerEventKinds(t, f, "run_epic")
	assert.Equal(t, 1, countKind(kinds, "run_paused"))
	assert.Equal(t, 1, countKind(kinds, "run_resumed"))
	assert.Equal(t, 3, countKind(kinds, "agent_reported"))
}
