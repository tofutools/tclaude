package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// processJoinAnyTemplate races `branches` program tasks into one join: any
// reducer, whose own program is the downstream marker the acceptance criteria
// ask for: it runs exactly once, for the winner.
//
//	start -> fork -{b01..bNN}-> task-01..task-NN -> join(any) -> end
func processJoinAnyTemplate(id string, branches int) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "fork"}},
		"join": {
			Type: model.NodeTypeTask, Join: model.JoinAny, Next: model.Next{"next": "end"},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		},
		"end": {Type: model.NodeTypeEnd, Result: "success"},
	}
	fork := model.Next{}
	for i := 1; i <= branches; i++ {
		node := fmt.Sprintf("task-%02d", i)
		fork[fmt.Sprintf("b%02d", i)] = node
		nodes[node] = model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": "join"},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		}
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: fork}
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start", Nodes: nodes}
}

// TestProcessRuntimeJoinAnyWinnerRunsDownstreamOnceAndWaitsForLosers is the
// product shape TCL-715 exists for: the fast branch wins, its downstream route
// runs immediately and exactly once, the slow branches keep running untouched,
// and the run refuses to call itself complete until they have settled.
func TestProcessRuntimeJoinAnyWinnerRunsDownstreamOnceAndWaitsForLosers(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processJoinAnyTemplate("join-any", 3))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_join_any", "join-any")
	require.Equal(t, []string{"task-01", "task-02", "task-03"}, gate.awaitEntered(t, 3))

	// The first branch to report wins, and winning is what dispatches the
	// reducer's own program: it reaching the gate IS the downstream activation.
	gate.release("task-01")
	require.Equal(t, []string{"join"}, gate.awaitEntered(t, 1))

	won := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.EdgeArrived, won.Checkpoint.Edges["task-01"]["next"], "the first arrival is the winner")
	assert.Equal(t, engine.EdgeUnresolved, won.Checkpoint.Edges["task-02"]["next"])
	assert.Equal(t, engine.EdgeUnresolved, won.Checkpoint.Edges["task-03"]["next"])
	assert.Equal(t, map[string]string{
		"task-02": "executing", "task-03": "executing", "join": "executing",
	}, commandStates(won), "losing branches keep running beside the winner's downstream work")

	// The reducer finishes, so the run has reached its end node while two
	// dispatched losers are still outstanding. It must sit there rather than
	// claim a terminal outcome.
	gate.release("join")
	require.Eventually(t, func() bool {
		view := showProcessRun(t, f, run.ID)
		return view.Checkpoint.Nodes["join"] == engine.NodeDone &&
			view.Checkpoint.Nodes["end"] == engine.NodeReady
	}, 10*time.Second, 10*time.Millisecond, "the winner's downstream route did not settle")

	parked := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, parked.Status,
		"a run must not claim completion while dispatched loser work is outstanding")
	assert.Equal(t, map[string]string{"task-02": "executing", "task-03": "executing"}, commandStates(parked))
	assert.Equal(t, int32(4), gate.performs.Load(), "three branches plus one downstream run")

	// The losers settle as late arrivals, and the last of them finalizes the run.
	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Empty(t, final.Commands)
	assert.Equal(t, engine.EdgeArrived, final.Checkpoint.Edges["task-01"]["next"],
		"a late arrival cannot replace the durable winner")
	assert.Equal(t, engine.EdgeArrivedLate, final.Checkpoint.Edges["task-02"]["next"])
	assert.Equal(t, engine.EdgeArrivedLate, final.Checkpoint.Edges["task-03"]["next"])
	for _, node := range []string{"task-01", "task-02", "task-03", "join", "end"} {
		assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes[node], node)
	}
	assert.Equal(t, int32(4), gate.performs.Load(), "the winner's downstream route ran exactly once")

	// The evidence stream alone explains the outcome, with no SQLite access.
	won2, late := joinArrivalEvidence(t, f, run.ID)
	assert.Equal(t, []string{"task-01"}, won2, "exactly one branch is recorded as the winner")
	assert.ElementsMatch(t, []string{"task-02", "task-03"}, late,
		"losers are recorded in whatever order they finish")

	// And it reads in causal order: the winning branch reported, that won the
	// join, and only then was the reducer's own command prepared. The public
	// sequence is the human-facing history, so it must not claim the downstream
	// command existed before the join it depends on was decided.
	sequence := processRunEventSequence(t, f, run.ID)
	observedWinner := indexOfEvent(sequence, "program_observed", "task-01")
	joinWon := indexOfEvent(sequence, "join_won", "join")
	joinPrepared := indexOfEvent(sequence, "program_prepared", "join")
	require.NotEqual(t, -1, observedWinner)
	require.NotEqual(t, -1, joinWon)
	require.NotEqual(t, -1, joinPrepared)
	assert.Less(t, observedWinner, joinWon, "the arrival is recorded after the input that caused it")
	assert.Less(t, joinWon, joinPrepared, "the downstream command is recorded after the join it needed")
}

// TestProcessRuntimeJoinAnySimultaneousArrivalsElectOneWinner releases two
// branches at once so their observations genuinely race into the run's single
// state owner. Whichever it serializes first, exactly one edge may claim the
// join and the reducer may run only once.
func TestProcessRuntimeJoinAnySimultaneousArrivalsElectOneWinner(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processJoinAnyTemplate("join-any-race", 2))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_join_any_race", "join-any-race")
	require.Equal(t, []string{"task-01", "task-02"}, gate.awaitEntered(t, 2))
	// Both branches leave the program boundary together, so their observations
	// really do race into the owner rather than arriving in a staged order.
	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	winners, late := 0, 0
	for _, node := range []string{"task-01", "task-02"} {
		switch final.Checkpoint.Edges[node]["next"] {
		case engine.EdgeArrived:
			winners++
		case engine.EdgeArrivedLate:
			late++
		default:
			t.Fatalf("branch %q edge = %q", node, final.Checkpoint.Edges[node]["next"])
		}
	}
	assert.Equal(t, 1, winners, "a race must not elect two winners")
	assert.Equal(t, 1, late)
	assert.Equal(t, int32(3), gate.performs.Load(), "two branches plus one downstream run")
	won, lateNodes := joinArrivalEvidence(t, f, run.ID)
	assert.Len(t, won, 1)
	assert.Len(t, lateNodes, 1)
}

// TestProcessRuntimeJoinAnyCapacityQueuedLosersAreDispatchedNotCancelled covers
// the loser branches the bounded runtime had not started yet when the join was
// decided. They are not a graph cut to compute: they stay ready, get dispatched
// as slots free, and the run waits for them like any other outstanding work.
func TestProcessRuntimeJoinAnyCapacityQueuedLosersAreDispatchedNotCancelled(t *testing.T) {
	f, root := processRuntimeFlow(t)
	concurrency := agentd.ProcessRunConcurrencyForTest()
	branches := concurrency + 2
	putProcessRuntimeTemplate(t, root, processJoinAnyTemplate("join-any-capacity", branches))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_join_any_capacity", "join-any-capacity")
	started := gate.awaitEntered(t, concurrency)
	require.Len(t, started, concurrency)
	queued := showProcessRun(t, f, run.ID)
	ready := 0
	for _, status := range queued.Checkpoint.Nodes {
		if status == engine.NodeReady {
			ready++
		}
	}
	require.Equal(t, branches-concurrency, ready, "the fixture must actually queue losers behind capacity")

	// The winner decides the join while two of its rivals have not even started.
	gate.release(started[0])
	require.Eventually(t, func() bool {
		return showProcessRun(t, f, run.ID).Checkpoint.Edges[started[0]]["next"] == engine.EdgeArrived
	}, 10*time.Second, 10*time.Millisecond, "the first arrival did not win the join")

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, int32(branches+1), gate.performs.Load(),
		"every branch ran exactly once, including the ones queued behind capacity when the join was decided")
	for i := 1; i <= branches; i++ {
		node := fmt.Sprintf("task-%02d", i)
		assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes[node], "%s was silently cancelled", node)
	}
	won, late := joinArrivalEvidence(t, f, run.ID)
	assert.Equal(t, []string{started[0]}, won)
	assert.Len(t, late, branches-1)
}

// TestProcessRuntimeJoinAnyRestartPreservesTheWinnerAndInventsNoWork crashes the
// daemon after the winner was chosen but before the losers finished. The winner
// survives, nothing is re-elected, no reconciliation work is invented, and the
// losers stay individually reconcilable exactly as any outstanding command does.
func TestProcessRuntimeJoinAnyRestartPreservesTheWinnerAndInventsNoWork(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processJoinAnyTemplate("join-any-restart", 3))
	gate := newProcessProgramGate(t)
	// The losers' programs are lost rather than observed — the daemon dies
	// without ever learning what they did, which is the honest crash shape.
	gate.silentOn("task-02", "task-03")

	run := createProcessRun(t, f, "run_join_any_restart", "join-any-restart")
	gate.awaitEntered(t, 3)
	gate.release("task-01")
	require.Equal(t, []string{"join"}, gate.awaitEntered(t, 1))
	gate.release("join", "task-02", "task-03")
	agentd.WaitForProcessRunRuntimeForTest()

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	before := gate.performs.Load()
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, before, gate.performs.Load(),
		"recovery re-dispatched work; a decided join must not be re-run or re-elected")

	cold := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunRunning, cold.Status, "the run is not complete while losers are outstanding")
	assert.Equal(t, engine.EdgeArrived, cold.Checkpoint.Edges["task-01"]["next"], "restart lost the winner")
	assert.Equal(t, engine.NodeDone, cold.Checkpoint.Nodes["join"])
	assert.Equal(t, engine.NodeReady, cold.Checkpoint.Nodes["end"])
	assert.Equal(t, map[string]string{
		"task-02": "needs_reconcile", "task-03": "needs_reconcile",
	}, commandStates(cold), "a loser is an ordinary outstanding command, not reconciliation work of its own")

	// Reconciling them out of band settles them as late arrivals and finalizes
	// the run — the winner is still the winner.
	for _, node := range []string{"task-02", "task-03"} {
		recorded := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/record-outcome",
			map[string]any{"nodeId": node, "outcome": "succeeded", "note": "confirmed out of band"})
		require.Equal(t, http.StatusAccepted, recorded.Code, recorded.Body.String())
		agentd.WaitForProcessRunRuntimeForTest()
	}
	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, engine.EdgeArrived, final.Checkpoint.Edges["task-01"]["next"])
	assert.Equal(t, engine.EdgeArrivedLate, final.Checkpoint.Edges["task-02"]["next"])
	assert.Equal(t, engine.EdgeArrivedLate, final.Checkpoint.Edges["task-03"]["next"])
	assert.Equal(t, before, gate.performs.Load())
	won, late := joinArrivalEvidence(t, f, run.ID)
	assert.Equal(t, []string{"task-01"}, won)
	assert.ElementsMatch(t, []string{"task-02", "task-03"}, late)
}

// processRunEvent is one row of the public evidence stream, reduced to what an
// ordering assertion needs.
type processRunEvent struct {
	Kind   string
	NodeID string
}

// processRunEventSequence reads the whole public evidence stream in sequence
// order, through the same paged API an operator would use.
func processRunEventSequence(t *testing.T, f *testharness.Flow, runID string) []processRunEvent {
	t.Helper()
	var sequence []processRunEvent
	for cursor := int64(0); ; {
		url := fmt.Sprintf("/v1/process/runs/%s/events?limit=16&after=%d", runID, cursor)
		response := processRuntimeRequest(t, f, http.MethodGet, url, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var page processRuntimeEventPage
		testharness.DecodeJSON(t, response, &page)
		for _, event := range page.Events {
			sequence = append(sequence, processRunEvent{Kind: event.Kind, NodeID: event.NodeID})
		}
		if len(page.Events) == 0 || page.Next <= cursor {
			return sequence
		}
		cursor = page.Next
	}
}

func indexOfEvent(sequence []processRunEvent, kind, nodeID string) int {
	for index, event := range sequence {
		if event.Kind == kind && event.NodeID == nodeID {
			return index
		}
	}
	return -1
}

// joinArrivalEvidence reads the run's public evidence stream and returns which
// branches it records as having won a join and which as having arrived late.
// It deliberately goes through the same paged API an operator would use.
func joinArrivalEvidence(t *testing.T, f *testharness.Flow, runID string) (won, late []string) {
	t.Helper()
	for cursor := int64(0); ; {
		url := fmt.Sprintf("/v1/process/runs/%s/events?limit=16&after=%d", runID, cursor)
		response := processRuntimeRequest(t, f, http.MethodGet, url, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var page processRuntimeEventPage
		testharness.DecodeJSON(t, response, &page)
		for _, event := range page.Events {
			if event.Kind != "join_won" && event.Kind != "join_arrival_late" {
				continue
			}
			var payload struct {
				JoinNodeID string `json:"joinNodeId"`
				From       string `json:"from"`
				Outcome    string `json:"outcome"`
			}
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			require.Equal(t, event.NodeID, payload.JoinNodeID, "join evidence names its reducer")
			require.NotEmpty(t, payload.Outcome)
			if event.Kind == "join_won" {
				won = append(won, payload.From)
			} else {
				late = append(late, payload.From)
			}
		}
		if len(page.Events) == 0 || page.Next <= cursor {
			return won, late
		}
		cursor = page.Next
	}
}
