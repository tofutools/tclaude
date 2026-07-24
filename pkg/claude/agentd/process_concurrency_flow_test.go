package agentd_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// processFanOutTemplate fans out to `branches` program tasks that all reduce at
// a join: all end:
//
//	start -> fork -{b01..bNN}-> task-01..task-NN -> end(join: all)
func processFanOutTemplate(id string, branches int) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "fork"}},
		"end":   {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "success"},
	}
	fork := model.Next{}
	for i := 1; i <= branches; i++ {
		node := fmt.Sprintf("task-%02d", i)
		fork[fmt.Sprintf("b%02d", i)] = node
		nodes[node] = model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		}
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: fork}
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start", Nodes: nodes}
}

// processMixedFanOutTemplate parks one branch on a human while the others run
// programs — the exact shape PR1 could not serve, because the run stayed
// claimed for as long as the sibling programs ran.
//
//	start -> fork -{decide, b01..bNN}-> decide / task-01..task-NN -> end(join: all)
func processMixedFanOutTemplate(id string, branches int) *model.Template {
	tmpl := processFanOutTemplate(id, branches)
	fork := tmpl.Nodes["fork"]
	fork.Next["decide"] = "decide"
	tmpl.Nodes["fork"] = fork
	tmpl.Nodes["decide"] = model.Node{
		Type:      model.NodeTypeDecision,
		Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"},
		Next:      model.Next{"approve": "end", "reject": "end"},
	}
	return tmpl
}

// processProgramGate holds every worker at the external-program boundary until
// the test releases it by node id. It is what makes the concurrency assertions
// exact rather than timing-based: a branch is executing precisely while it sits
// in the gate, so "four at once" is observed, not inferred from a sleep.
type processProgramGate struct {
	mu       sync.Mutex
	releases map[string]chan struct{}
	failing  map[string]bool
	silent   map[string]bool
	open     bool
	entered  chan string
	performs atomic.Int32
}

func newProcessProgramGate(t *testing.T) *processProgramGate {
	t.Helper()
	gate := &processProgramGate{
		releases: map[string]chan struct{}{},
		failing:  map[string]bool{},
		silent:   map[string]bool{},
		entered:  make(chan string, 64),
	}
	t.Cleanup(agentd.SetProcessProgramPerformForTest(gate.perform))
	// Every gate is released at teardown so a test that fails an assertion mid
	// scenario still lets the daemon's owners drain instead of hanging.
	t.Cleanup(gate.releaseAll)
	return gate
}

func (g *processProgramGate) perform(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
	command := dispatch.Command()
	g.performs.Add(1)
	g.entered <- command.NodeID
	canceled := false
	select {
	case <-g.gateFor(command.NodeID):
	case <-ctx.Done():
		canceled = true
	}
	g.mu.Lock()
	failing, silent := g.failing[command.NodeID], g.silent[command.NodeID]
	g.mu.Unlock()
	if silent {
		// Models a worker that never produced an observation at all — the
		// daemon lost the program rather than observing it.
		return executor.Result{}, fmt.Errorf("no observation for %s", command.NodeID)
	}
	if canceled {
		// A cancelled worker still reports honestly: a killed program is a
		// failed observation, not a missing one.
		return executor.Result{
			Observation: engine.ProgramObservation{
				CommandID: command.ID, NodeID: command.NodeID,
				Outcome: engine.ProgramFailed, ExitCode: -1,
				Error: "program canceled: " + ctx.Err().Error(),
			},
			Canceled: true,
		}, nil
	}
	observation := engine.ProgramObservation{
		CommandID: command.ID, NodeID: command.NodeID, Outcome: engine.ProgramSucceeded,
	}
	if failing {
		observation.Outcome, observation.ExitCode, observation.Error = engine.ProgramFailed, 3, "branch failed"
	}
	return executor.Result{Observation: observation, Dispatched: true}, nil
}

func (g *processProgramGate) gateFor(nodeID string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.releases[nodeID] == nil {
		gate := make(chan struct{})
		if g.open {
			// Once the gate is open, branches that start later must not block:
			// the test has already said it wants the run to finish.
			close(gate)
		}
		g.releases[nodeID] = gate
	}
	return g.releases[nodeID]
}

func (g *processProgramGate) failOn(nodeIDs ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, nodeID := range nodeIDs {
		g.failing[nodeID] = true
	}
}

func (g *processProgramGate) silentOn(nodeIDs ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, nodeID := range nodeIDs {
		g.silent[nodeID] = true
	}
}

// reportOn undoes silentOn, so a later attempt at the same branch reports
// normally.
func (g *processProgramGate) reportOn(nodeIDs ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, nodeID := range nodeIDs {
		delete(g.silent, nodeID)
	}
}

func (g *processProgramGate) release(nodeIDs ...string) {
	for _, nodeID := range nodeIDs {
		gate := g.gateFor(nodeID)
		select {
		case <-gate:
		default:
			close(gate)
		}
	}
}

// releaseAll releases every branch that is waiting AND every branch that starts
// afterwards, so a test can say "let the rest of the run finish" once.
func (g *processProgramGate) releaseAll() {
	g.mu.Lock()
	g.open = true
	nodes := make([]string, 0, len(g.releases))
	for nodeID := range g.releases {
		nodes = append(nodes, nodeID)
	}
	g.mu.Unlock()
	g.release(nodes...)
}

// awaitEntered blocks until exactly n workers have reached the program
// boundary and returns their node ids sorted.
func (g *processProgramGate) awaitEntered(t *testing.T, n int) []string {
	t.Helper()
	nodes := make([]string, 0, n)
	for range n {
		select {
		case node := <-g.entered:
			nodes = append(nodes, node)
		case <-t.Context().Done():
			t.Fatalf("only %d of %d workers reached the program boundary", len(nodes), n)
		}
	}
	sort.Strings(nodes)
	return nodes
}

func showProcessRun(t *testing.T, f *testharness.Flow, runID string) processRuntimeRunView {
	t.Helper()
	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var view processRuntimeRunView
	testharness.DecodeJSON(t, show, &view)
	return view
}

func createProcessRun(t *testing.T, f *testharness.Flow, id, templateID string) processRuntimeRunView {
	t.Helper()
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": id, "templateId": templateID, "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var view processRuntimeRunView
	testharness.DecodeJSON(t, created, &view)
	return view
}

func commandStates(view processRuntimeRunView) map[string]string {
	states := make(map[string]string, len(view.Commands))
	for _, command := range view.Commands {
		states[command.NodeID] = command.State
	}
	return states
}

// TestProcessRuntimeBoundedConcurrencyStartsAtMostKAndRefillsOneSlot is the
// core capacity proof: a fork wider than K starts exactly K external programs
// at once, the ready remainder stays ready with no durable queue, and finishing
// one branch admits exactly one more.
func TestProcessRuntimeBoundedConcurrencyStartsAtMostKAndRefillsOneSlot(t *testing.T) {
	f, root := processRuntimeFlow(t)
	concurrency := agentd.ProcessRunConcurrencyForTest()
	branches := concurrency + 3
	putProcessRuntimeTemplate(t, root, processFanOutTemplate("wide", branches))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_wide", "wide")
	first := gate.awaitEntered(t, concurrency)
	require.Len(t, first, concurrency)

	// Exactly K commands are durable and executing; the rest of the fork is
	// ready and waiting for a slot, with nothing queued anywhere.
	view := showProcessRun(t, f, run.ID)
	assert.Len(t, view.Checkpoint.Commands, concurrency, "capacity bounds the durable outbox too")
	assert.Len(t, view.Commands, concurrency)
	for _, command := range view.Commands {
		assert.Equal(t, "executing", command.State, command.NodeID)
	}
	assert.Equal(t, "executing", view.Action)
	ready := 0
	for node, status := range view.Checkpoint.Nodes {
		if status == engine.NodeReady {
			ready++
			assert.NotContains(t, first, node, "an executing branch is running, not ready")
		}
	}
	assert.Equal(t, branches-concurrency, ready, "ready branches past capacity simply stay ready")
	assert.Equal(t, int32(concurrency), gate.performs.Load())

	// One completion refills exactly one slot.
	gate.release(first[0])
	refilled := gate.awaitEntered(t, 1)
	assert.NotContains(t, first, refilled[0], "the freed slot admits a branch that was still ready")
	assert.Equal(t, int32(concurrency+1), gate.performs.Load(),
		"a single completion must not admit more than a single replacement")
	assert.Len(t, showProcessRun(t, f, run.ID).Commands, concurrency,
		"the outbox stays at capacity, never above it")

	gate.releaseAll()
	for range branches - concurrency - 1 {
		gate.awaitEntered(t, 1)
	}
	agentd.WaitForProcessRunRuntimeForTest()
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Empty(t, final.Commands)
	assert.Equal(t, int32(branches), gate.performs.Load(), "every branch runs exactly once")
}

// TestProcessRuntimeConcurrentBranchesReallyOverlap proves overlap rather than
// interleaving: K workers are simultaneously inside their programs, which is
// only observable if none of them had to finish before the next one started.
// No timing is involved — the daemon cannot make progress until the test
// releases them.
func TestProcessRuntimeConcurrentBranchesReallyOverlap(t *testing.T) {
	f, root := processRuntimeFlow(t)
	concurrency := agentd.ProcessRunConcurrencyForTest()
	putProcessRuntimeTemplate(t, root, processFanOutTemplate("overlap", concurrency))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_overlap", "overlap")
	entered := gate.awaitEntered(t, concurrency)

	// All K are inside their program at the same instant: none can have
	// returned, because none has been released.
	assert.Equal(t, entered, agentd.ProcessRunExecutingNodesForTest(run.ID))
	states := commandStates(showProcessRun(t, f, run.ID))
	require.Len(t, states, concurrency)
	for _, node := range entered {
		assert.Equal(t, "executing", states[node], node)
	}

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Empty(t, agentd.ProcessRunExecutingNodesForTest(run.ID))
}

// TestProcessRuntimeDecisionSucceedsWhileSiblingProgramsExecute is the
// regression this PR exists for. Under PR1 the run stayed claimed for the whole
// chained program sequence, so an awaited sibling decision was invisible to
// nothing and POST /decide answered 409 process_run_claimed for as long as
// those programs ran — up to one hour each.
func TestProcessRuntimeDecisionSucceedsWhileSiblingProgramsExecute(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processMixedFanOutTemplate("decide-while-running", 2))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_decide_running", "decide-while-running")
	executing := gate.awaitEntered(t, 2)
	require.Equal(t, []string{"task-01", "task-02"}, executing)

	// Both outboxes are visible at once: the programs AND the parked human.
	view := showProcessRun(t, f, run.ID)
	assert.Equal(t, "executing", view.Action)
	assert.Equal(t, map[string]string{"task-01": "executing", "task-02": "executing"}, commandStates(view))
	require.Len(t, view.AwaitingDecisions, 1)
	assert.Equal(t, "decide", view.AwaitingDecisions[0].NodeID)
	assert.Equal(t, []string{"approve", "reject"}, view.AwaitingDecisions[0].Verdicts)
	require.NotNil(t, view.AwaitingDecision, "the singular field still serves older clients")
	assert.Equal(t, "decide", view.AwaitingDecision.NodeID)

	// The decision is addressable and succeeds while both programs are still
	// blocked inside their workers.
	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "decide", "verdict": "approve", "evidence": "checked while tasks ran"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
	assert.NotContains(t, decide.Body.String(), "process_run_claimed")
	require.Equal(t, []string{"task-01", "task-02"}, agentd.ProcessRunExecutingNodesForTest(run.ID),
		"the verdict committed without disturbing either running branch")

	decided := showProcessRun(t, f, run.ID)
	assert.Empty(t, decided.AwaitingDecisions)
	assert.Equal(t, engine.NodeDone, decided.Checkpoint.Nodes["decide"])
	assert.Equal(t, engine.EdgeArrived, decided.Checkpoint.Edges["decide"]["approve"])
	assert.Len(t, decided.Commands, 2, "deciding a sibling must not touch the running branches")

	// A duplicate verdict is still refused, and still while the run is claimed.
	duplicate := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "decide", "verdict": "reject"})
	assert.Equal(t, http.StatusConflict, duplicate.Code, duplicate.Body.String())
	assert.Contains(t, duplicate.Body.String(), `"code":"process_decision_stale"`)

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)

	// The verdict was recorded against the authenticated caller, in order.
	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID+"/events", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	decisions := 0
	for _, event := range page.Events {
		if event.Kind == "decision_recorded" {
			decisions++
			assert.Equal(t, "decide", event.NodeID)
			assert.NotEmpty(t, event.Actor, "the actor is the authenticated caller")
			assert.Contains(t, string(event.Payload), `"verdict":"approve"`)
		}
	}
	assert.Equal(t, 1, decisions)
}

// TestProcessRuntimeDecisionAndProgramResultSerializeWithoutLostUpdate drives a
// verdict and a program result at the same owner concurrently. Whichever the
// owner takes first, both commit exactly once and neither overwrites the other.
func TestProcessRuntimeDecisionAndProgramResultSerializeWithoutLostUpdate(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processMixedFanOutTemplate("decide-race", 2))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_decide_race", "decide-race")
	gate.awaitEntered(t, 2)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var decision *httptest.ResponseRecorder
	wg.Add(2)
	go func() {
		defer wg.Done()
		request := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/process/runs/"+run.ID+"/decide", map[string]any{"nodeId": "decide", "verdict": "reject"}))
		<-start
		decision = testharness.Serve(f.Mux, request)
	}()
	go func() {
		defer wg.Done()
		<-start
		gate.release("task-01")
	}()
	close(start)
	wg.Wait()

	require.Equal(t, http.StatusAccepted, decision.Code, decision.Body.String())
	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["decide"])
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["task-01"])
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["task-02"])
	assert.Equal(t, engine.EdgeArrived, final.Checkpoint.Edges["decide"]["reject"])
	assert.Equal(t, engine.EdgeNotTaken, final.Checkpoint.Edges["decide"]["approve"])

	// Deterministic evidence: exactly one row per branch outcome and one
	// verdict, whichever order the owner serialized them in.
	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID+"/events?limit=16", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	counts := map[string]int{}
	for _, event := range page.Events {
		counts[event.Kind]++
	}
	assert.Equal(t, 1, counts["decision_recorded"], "no lost or duplicated verdict")
	assert.Equal(t, 1, counts["decision_awaited"])
	assert.Equal(t, 2, counts["program_prepared"])
	assert.Equal(t, 2, counts["program_observed"], "no lost or duplicated observation")
}

// TestProcessRuntimeColdOutstandingCommandsAreAllShownAndIndividuallyResolved
// covers the plural reconciliation surface: several commands survive a restart,
// every one of them is presented, a mutation that does not say which one it
// means fails closed, and each is resolvable on its own identity.
func TestProcessRuntimeColdOutstandingCommandsAreAllShownAndIndividuallyResolved(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processFanOutTemplate("cold-plural", 3))
	gate := newProcessProgramGate(t)
	// The daemon dies without any of the three workers reporting, which is what
	// makes all three commands ambiguous on the next load.
	gate.silentOn("task-01", "task-02", "task-03")

	run := createProcessRun(t, f, "run_cold_plural", "cold-plural")
	gate.awaitEntered(t, 3)

	// Model the daemon dying with three commands durable and in flight.
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	gate.releaseAll()

	cold := showProcessRun(t, f, run.ID)
	require.Len(t, cold.Commands, 3)
	assert.True(t, cold.NeedsReconcile)
	assert.Equal(t, "needs_reconcile", cold.Action)
	for _, command := range cold.Commands {
		assert.Equal(t, "needs_reconcile", command.State, command.NodeID)
		assert.NotEmpty(t, command.CommandID)
		assert.Equal(t, "safe", command.Profile)
	}

	// The bounded sweep must never redispatch any of them.
	before := gate.performs.Load()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, before, gate.performs.Load(), "cold outstanding commands are never redispatched")

	// With three candidates, an unselected mutation fails closed.
	for _, route := range []struct {
		path string
		body map[string]any
	}{
		{"/reissue", map[string]any{}},
		{"/record-outcome", map[string]any{"outcome": "succeeded"}},
	} {
		ambiguous := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+route.path, route.body)
		assert.Equal(t, http.StatusConflict, ambiguous.Code, ambiguous.Body.String())
		assert.Contains(t, ambiguous.Body.String(), `"code":"process_run_reconcile_ambiguous"`)
	}
	unknown := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/record-outcome",
		map[string]any{"nodeId": "task-99", "outcome": "succeeded"})
	assert.Equal(t, http.StatusConflict, unknown.Code, unknown.Body.String())
	assert.Contains(t, unknown.Body.String(), `"code":"process_run_not_reconcilable"`)

	// Each command is resolvable by naming it, and resolving one leaves the
	// others exactly as they were.
	resolved := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/record-outcome",
		map[string]any{"nodeId": "task-01", "outcome": "succeeded", "note": "confirmed out of band"})
	require.Equal(t, http.StatusAccepted, resolved.Code, resolved.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, map[string]string{
		"task-02": "needs_reconcile", "task-03": "needs_reconcile",
	}, commandStates(showProcessRun(t, f, run.ID)))

	// Down to two, still ambiguous; down to one, the convenience form returns.
	require.Equal(t, http.StatusAccepted, processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/"+run.ID+"/record-outcome",
		map[string]any{"nodeId": "task-02", "outcome": "succeeded"}).Code)
	agentd.WaitForProcessRunRuntimeForTest()
	last := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/record-outcome",
		map[string]any{"outcome": "succeeded"})
	require.Equal(t, http.StatusAccepted, last.Code, last.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Empty(t, final.Commands)
	assert.Equal(t, before, gate.performs.Load(), "reconciliation never re-ran a program")
}

// TestProcessRuntimeSingleColdCommandKeepsTheNoSelectorConvenience pins the
// other side of the selector rule: with exactly one candidate, the plural
// surface must not make the operator name it.
func TestProcessRuntimeSingleColdCommandKeepsTheNoSelectorConvenience(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("cold-single", 1))
	gate := newProcessProgramGate(t)
	gate.silentOn("task-01")

	run := createProcessRun(t, f, "run_cold_single", "cold-single")
	gate.awaitEntered(t, 1)
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	gate.releaseAll()

	require.Len(t, showProcessRun(t, f, run.ID).Commands, 1)
	gate.reportOn("task-01")
	reissued := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/reissue", map[string]any{})
	require.Equal(t, http.StatusAccepted, reissued.Code, reissued.Body.String())
	gate.awaitEntered(t, 1)
	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, engine.RunCompleted, showProcessRun(t, f, run.ID).Status)
}

// TestProcessRuntimeFirstBranchFailureDrainsSiblingsBeforeFinalizing covers the
// whole failure rule: the first observed failure stops planning and cancels the
// siblings best-effort, the run stays running while their outcomes are still
// owed, no sibling command is erased before it is accounted for, awaited
// decisions are abandoned at once, and only a drained outbox finalizes failed.
func TestProcessRuntimeFirstBranchFailureDrainsSiblingsBeforeFinalizing(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processMixedFanOutTemplate("failure-drain", 3))
	gate := newProcessProgramGate(t)
	gate.failOn("task-01")

	run := createProcessRun(t, f, "run_failure_drain", "failure-drain")
	require.Equal(t, []string{"task-01", "task-02", "task-03"}, gate.awaitEntered(t, 3))
	require.Len(t, showProcessRun(t, f, run.ID).AwaitingDecisions, 1)

	// The failing branch reports; its siblings are cancelled but still owe
	// observations, so the run must not be called failed yet.
	gate.release("task-01")
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunFailed, final.Status, "the run finalizes once every command drained")
	assert.Equal(t, "terminal", final.Action)
	assert.Empty(t, final.Commands, "a terminal run strands no command")
	assert.Empty(t, final.AwaitingDecisions, "a failing run abandons its awaited decisions")
	assert.Equal(t, engine.NodeFailed, final.Checkpoint.Nodes["task-01"])

	// Every branch that was in flight produced an accounted-for outcome; none
	// was silently erased, and nothing new was planned after the failure.
	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID+"/events?limit=16", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	observed := map[string]bool{}
	for _, event := range page.Events {
		if event.Kind == "program_observed" {
			observed[event.NodeID] = true
		}
	}
	assert.Equal(t, map[string]bool{"task-01": true, "task-02": true, "task-03": true}, observed,
		"every dispatched sibling is accounted for by its own observation")
	assert.Equal(t, int32(3), gate.performs.Load(), "a failed run plans nothing further")

	// A verdict for the abandoned obligation now fails closed.
	late := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "decide", "verdict": "approve"})
	assert.Equal(t, http.StatusConflict, late.Code, late.Body.String())
	assert.Contains(t, late.Body.String(), `"code":"process_decision_stale"`)
}

// TestProcessRuntimeCrashDuringFailureDrainKeepsSiblingsReconcilable is the
// honesty half of the failure rule: if the daemon dies while a failed run is
// still draining, the unresolved siblings cold-load as needs_reconcile rather
// than being assumed succeeded, lost, or redispatched.
func TestProcessRuntimeCrashDuringFailureDrainKeepsSiblingsReconcilable(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processFanOutTemplate("failure-crash", 3))
	gate := newProcessProgramGate(t)
	gate.failOn("task-01")
	// The siblings never report, so the failure cannot drain on its own.
	gate.silentOn("task-02", "task-03")

	run := createProcessRun(t, f, "run_failure_crash", "failure-crash")
	gate.awaitEntered(t, 3)
	gate.release("task-01")
	agentd.WaitForProcessRunRuntimeForTest()

	draining := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, draining.Status,
		"a failed node does not make the run terminal while commands are unresolved")
	assert.Equal(t, engine.NodeFailed, draining.Checkpoint.Nodes["task-01"])
	assert.Equal(t, map[string]string{
		"task-02": "needs_reconcile", "task-03": "needs_reconcile",
	}, commandStates(draining))

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	before := gate.performs.Load()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, before, gate.performs.Load(), "a draining failed run is never redispatched")

	cold := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, cold.Status)
	assert.Equal(t, "needs_reconcile", cold.Action)
	assert.Equal(t, map[string]string{
		"task-02": "needs_reconcile", "task-03": "needs_reconcile",
	}, commandStates(cold))

	// Reconciling the remaining commands finalizes the run as failed — the
	// failure is not forgotten just because the later branches succeeded.
	for _, node := range []string{"task-02", "task-03"} {
		recorded := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/record-outcome",
			map[string]any{"nodeId": node, "outcome": "succeeded", "note": "confirmed out of band"})
		require.Equal(t, http.StatusAccepted, recorded.Code, recorded.Body.String())
		agentd.WaitForProcessRunRuntimeForTest()
	}
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunFailed, final.Status)
	assert.Empty(t, final.Commands)
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["task-02"],
		"a late sibling success is real evidence and settles its own node")
	assert.Equal(t, before, gate.performs.Load())
}

// TestProcessRuntimeShutdownDuringConcurrentWorkersLeavesNothingAmbiguousLost
// proves the graceful-stop contract under concurrency: every in-flight branch
// ends up either with a committed observation or as a durable command that
// explicitly needs reconciliation. Nothing is silently dropped, and the
// production shutdown really drains.
func TestProcessRuntimeShutdownDuringConcurrentWorkersLeavesNothingAmbiguousLost(t *testing.T) {
	f, root := processRuntimeFlow(t)
	concurrency := agentd.ProcessRunConcurrencyForTest()
	putProcessRuntimeTemplate(t, root, processFanOutTemplate("shutdown-wide", concurrency))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_shutdown_wide", "shutdown-wide")
	entered := gate.awaitEntered(t, concurrency)
	require.Len(t, entered, concurrency)

	// Production shutdown: cancel every claim and wait for the owners to drain.
	remaining, restore, err := agentd.ShutdownProcessRunRuntimeForTest()
	t.Cleanup(restore)
	require.NoError(t, err)
	assert.Zero(t, remaining, "shutdown released every claim")
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())

	// The gate reports cancellation as a real failed observation, so every
	// branch committed one and the run finalized honestly.
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunFailed, final.Status)
	assert.Empty(t, final.Commands, "no branch was left both uncommitted and unrecorded")
	failed := 0
	for _, status := range final.Checkpoint.Nodes {
		if status == engine.NodeFailed {
			failed++
		}
	}
	assert.GreaterOrEqual(t, failed, 1, "cancellation is recorded as a failure, not silently ignored")
}

// TestProcessRuntimeShutdownWithUnreportedWorkersLeavesReconcilableCommands is
// the other shutdown outcome: a worker that returns no observation at all
// leaves its durable command outstanding for an operator, never redispatched.
func TestProcessRuntimeShutdownWithUnreportedWorkersLeavesReconcilableCommands(t *testing.T) {
	f, root := processRuntimeFlow(t)
	concurrency := agentd.ProcessRunConcurrencyForTest()
	putProcessRuntimeTemplate(t, root, processFanOutTemplate("shutdown-silent", concurrency))
	gate := newProcessProgramGate(t)
	for i := 1; i <= concurrency; i++ {
		gate.silentOn(fmt.Sprintf("task-%02d", i))
	}

	run := createProcessRun(t, f, "run_shutdown_silent", "shutdown-silent")
	gate.awaitEntered(t, concurrency)
	gate.releaseAll()

	remaining, restore, err := agentd.ShutdownProcessRunRuntimeForTest()
	t.Cleanup(restore)
	require.NoError(t, err)
	assert.Zero(t, remaining)

	view := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, view.Status)
	assert.Equal(t, "needs_reconcile", view.Action)
	require.Len(t, view.Commands, concurrency)
	for _, command := range view.Commands {
		assert.Equal(t, "needs_reconcile", command.State, command.NodeID)
	}
	before := gate.performs.Load()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, before, gate.performs.Load(), "the sweep never redispatches an ambiguous command")
}

// TestProcessRuntimeFallbackSweepFindsReadyWorkBesideAwaitedDecision is the PR1
// regression that must survive: a run holding a ready branch beside a parked
// human is still rediscovered by the bounded sweep, while a genuinely
// decision-quiescent run is not reloaded on every tick.
func TestProcessRuntimeFallbackSweepFindsReadyWorkBesideAwaitedDecision(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processMixedFanOutTemplate("sweep-mixed", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	quiescent := processDecisionTemplate("sweep-quiescent")
	quiescentRecord := putProcessRuntimeTemplate(t, root, quiescent)

	// A run parked mid-fork: the decision branch awaits a human while the task
	// branch is ready but unplanned, exactly as a crash right after the fork
	// would leave it.
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_sweep_mixed", definition)
	require.NoError(t, err)
	checkpoint, err = engine.AdvanceUntilQuiescent(checkpoint, definition)
	require.NoError(t, err)
	require.Len(t, checkpoint.AwaitingDecisions, 1)
	require.Equal(t, engine.NodeReady, checkpoint.Nodes["task-01"])
	createProcessRunFixtureWithCheckpoint(t, "run_sweep_mixed", record.Ref, tmpl, checkpoint)

	// A run parked purely on a human: nothing to plan, so the sweep must leave
	// it alone rather than reloading and re-preparing it every tick.
	quiescentCheckpoint, err := engine.Prepare(quiescent, map[string]string{})
	require.NoError(t, err)
	parked, err := engine.Initialize("run_sweep_quiescent", quiescentCheckpoint)
	require.NoError(t, err)
	parked, err = engine.AdvanceUntilQuiescent(parked, quiescentCheckpoint)
	require.NoError(t, err)
	require.Len(t, parked.AwaitingDecisions, 1)
	createProcessRunFixtureWithCheckpoint(t, "run_sweep_quiescent", quiescentRecord.Ref, quiescent, parked)
	quiescentBefore := showProcessRun(t, f, "run_sweep_quiescent").StateVersion

	gate := newProcessProgramGate(t)
	agentd.RunProcessRunSweepForTest()
	require.Equal(t, []string{"task-01"}, gate.awaitEntered(t, 1),
		"a ready branch beside an awaited decision must be rediscovered")
	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()

	assert.Equal(t, quiescentBefore, showProcessRun(t, f, "run_sweep_quiescent").StateVersion,
		"a decision-quiescent run is not reloaded and re-prepared by the sweep")
	assert.Equal(t, int32(1), gate.performs.Load(), "only the run with ready work was driven")
}

// TestProcessRuntimeDaemonWideCapacityStillBoundsConcurrentRuns keeps the
// per-daemon claim ceiling meaningful now that each claim can hold several
// programs: a run that arrives with every claim taken stays runnable and is
// picked up by the bounded fallback sweep later.
func TestProcessRuntimeDaemonWideCapacityStillBoundsConcurrentRuns(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("claim-capacity", 1))
	gate := newProcessProgramGate(t)

	for i := range db.MaxProcessRunReadPage {
		createProcessRun(t, f, fmt.Sprintf("run_claim_%02d", i), "claim-capacity")
	}
	gate.awaitEntered(t, db.MaxProcessRunReadPage)
	require.Equal(t, db.MaxProcessRunReadPage, agentd.ProcessRunClaimCountForTest())

	deferred := createProcessRun(t, f, "run_claim_deferred", "claim-capacity")
	assert.Equal(t, "runnable", deferred.Action, "a capacity-deferred run stays runnable, not lost")
	assert.Equal(t, db.MaxProcessRunReadPage, agentd.ProcessRunClaimCountForTest())

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	agentd.RunProcessRunSweepForTest()
	gate.awaitEntered(t, 1)
	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, engine.RunCompleted, showProcessRun(t, f, deferred.ID).Status)
}
