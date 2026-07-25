package agentd_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// An explicit start-typed node is optional in the authoring contract, so these
// templates hand the runtime an entry node that is a fork, a task, or a human
// decision. Each one was refused before a run could be created until TCL-721.

// processDirectParallelEntryTemplate reproduces the deployed
// `m2-parallel-joinall-validation` shape from the TCL-649 validation, without
// the explicit start node that had to be inserted to make it runnable:
//
//	start: fork -{a..}-> sleep-* -> join(all, program) -> end
//	             -{choice}-> operator-choice -{continue,stop}-> join
func processDirectParallelEntryTemplate(id string, branches int) *model.Template {
	program := func(next string) model.Node {
		return model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": next},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		}
	}
	join := program("end")
	join.Join = model.JoinAll
	fork := model.Next{"choice": "operator-choice"}
	nodes := map[string]model.Node{
		"operator-choice": {
			Type:      model.NodeTypeDecision,
			Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Continue?"},
			Next:      model.Next{"continue": "join", "stop": "join"},
		},
		"join": join,
		"end":  {Type: model.NodeTypeEnd, Result: "success"},
	}
	for i := 1; i <= branches; i++ {
		node := fmt.Sprintf("sleep-%02d", i)
		fork[fmt.Sprintf("b%02d", i)] = node
		nodes[node] = program("join")
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: fork}
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "fork", Nodes: nodes}
}

func processDirectTaskEntryTemplate(id string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "task-01",
		Nodes: map[string]model.Node{
			"task-01": {
				Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

func processDirectDecisionEntryTemplate(id string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "choose",
		Nodes: map[string]model.Node{
			"choose": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Approve?"},
				Next:      model.Next{"approve": "work", "reject": "canceled"},
			},
			"work": {
				Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
			},
			"end":      {Type: model.NodeTypeEnd, Result: "success"},
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
}

// TestProcessRuntimeDirectParallelEntryCompletesThroughJoinAll is the deployed
// failure this ticket exists for: `process run` refused this template outright,
// and inserting a redundant start node was the only way to run it. The fork is
// now the entry, every branch runs concurrently, the sibling verdict is
// addressable while they do, and the join: all reducer runs last.
func TestProcessRuntimeDirectParallelEntryCompletesThroughJoinAll(t *testing.T) {
	f, root := processRuntimeFlow(t)
	branches := agentd.ProcessRunConcurrencyForTest()
	putProcessRuntimeTemplate(t, root, processDirectParallelEntryTemplate("m2-parallel-joinall-validation", branches))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_direct_parallel_entry", "m2-parallel-joinall-validation")
	executing := gate.awaitEntered(t, branches)
	require.Len(t, executing, branches)

	view := showProcessRun(t, f, run.ID)
	assert.NotContains(t, view.Checkpoint.Nodes, "start", "the template has no start node")
	assert.Equal(t, engine.NodeDone, view.Checkpoint.Nodes["fork"], "the entry fork advanced on its own")
	assert.Len(t, commandStates(view), branches)
	require.Len(t, view.AwaitingDecisions, 1)
	assert.Equal(t, "operator-choice", view.AwaitingDecisions[0].NodeID)
	assert.Equal(t, []string{"continue", "stop"}, view.AwaitingDecisions[0].Verdicts)
	assert.Equal(t, engine.NodePending, view.Checkpoint.Nodes["join"], "the reducer waits for its whole candidate set")

	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "operator-choice", "verdict": "continue"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, "terminal", final.Action)
	assert.Empty(t, final.Commands)
	assert.Empty(t, final.AwaitingDecisions)
	for nodeID, status := range final.Checkpoint.Nodes {
		assert.Equal(t, engine.NodeDone, status, "node %q", nodeID)
	}
	assert.Equal(t, engine.EdgeArrived, final.Checkpoint.Edges["operator-choice"]["continue"])
	assert.Equal(t, engine.EdgeNotTaken, final.Checkpoint.Edges["operator-choice"]["stop"])
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())
}

// TestProcessRuntimeDirectTaskEntryDispatchesAndCompletes proves the simplest
// entry: creation plans the entry task itself, with no engine-owned advance in
// front of it.
func TestProcessRuntimeDirectTaskEntryDispatchesAndCompletes(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processDirectTaskEntryTemplate("direct-task-entry"))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_direct_task_entry", "direct-task-entry")
	assert.Equal(t, []string{"task-01"}, gate.awaitEntered(t, 1))
	assert.Equal(t, map[string]string{"task-01": "executing"}, commandStates(showProcessRun(t, f, run.ID)))

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["task-01"])
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["end"])
	assert.Empty(t, final.Commands)
}

// TestProcessRuntimeDirectDecisionEntryIsAwaitedAtCreationAndSurvivesRestart
// covers the entry kind that parks the run on a human before anything has run:
// the obligation and its evidence are durable at creation, a restarted daemon
// still surfaces and can resolve it, and nothing was ever dispatched for it.
func TestProcessRuntimeDirectDecisionEntryIsAwaitedAtCreationAndSurvivesRestart(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processDirectDecisionEntryTemplate("direct-decision-entry"))
	gate := newProcessProgramGate(t)

	run := createProcessRun(t, f, "run_direct_decision_entry", "direct-decision-entry")
	assert.Equal(t, "awaiting_decision", run.Action)
	require.NotNil(t, run.AwaitingDecision)
	assert.Equal(t, "choose", run.AwaitingDecision.NodeID)
	assert.Equal(t, []string{"approve", "reject"}, run.AwaitingDecision.Verdicts)
	assert.Equal(t, engine.NodeReady, run.Checkpoint.Nodes["choose"])
	assert.Empty(t, run.Commands)

	// Creation is the only thing that ever happened, so it has to be what
	// announces the obligation: no engine advance will. The caller asked for a
	// run; initialization is what created the obligation, so the two rows are
	// attributed to different actors.
	created := processRunEvents(t, f, run.ID)
	require.Len(t, created.Events, 2)
	assert.Equal(t, int64(1), created.Events[0].Sequence)
	assert.Equal(t, "run_created", created.Events[0].Kind)
	assert.Empty(t, created.Events[0].NodeID)
	creator := created.Events[0].Actor
	assert.NotEmpty(t, creator, "run_created is attributed to the authenticated caller")
	assert.NotEqual(t, executor.EngineActor, creator)

	assert.Equal(t, int64(2), created.Events[1].Sequence)
	assert.Equal(t, "decision_awaited", created.Events[1].Kind)
	assert.Equal(t, "choose", created.Events[1].NodeID)
	assert.JSONEq(t, `{"nodeId":"choose"}`, string(created.Events[1].Payload))
	assert.Equal(t, executor.EngineActor, created.Events[1].Actor,
		"an obligation the engine created is attributed to the engine, exactly as a downstream one is")

	// Restart: a fresh OS process rebuilds the mux and runs the recovery sweep.
	// An awaited decision is not runnable work, so nothing is dispatched.
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	fresh := runProcessRuntimeFreshHost(t, f.World.HomeDir, root, run.ID)
	assert.Equal(t, engine.RunRunning, fresh.Status)
	assert.Equal(t, "awaiting_decision", fresh.Action)
	require.NotNil(t, fresh.AwaitingDecision)
	assert.Equal(t, "choose", fresh.AwaitingDecision.NodeID)
	assert.Equal(t, []string{"approve", "reject"}, fresh.AwaitingDecision.Verdicts)
	assert.Zero(t, gate.performs.Load(), "an entry decision must not dispatch anything")
	assert.Equal(t, []string{"run_created", "decision_awaited"}, processRunEventKinds(t, f, run.ID),
		"a restart re-reads the durable obligation; it does not re-announce it")

	// A resume against the same standing obligation is a durable no-op too.
	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, resume.Code, resume.Body.String())
	assert.Equal(t, []string{"run_created", "decision_awaited"}, processRunEventKinds(t, f, run.ID))

	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve", "evidence": "answered after restart"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
	assert.Equal(t, []string{"work"}, gate.awaitEntered(t, 1))

	gate.releaseAll()
	agentd.WaitForProcessRunRuntimeForTest()

	final := showProcessRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["choose"])
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["work"])
	assert.Equal(t, engine.NodeSkipped, final.Checkpoint.Nodes["canceled"])
	settled := processRunEvents(t, f, run.ID)
	kinds := make([]string, 0, len(settled.Events))
	awaited := 0
	for _, event := range settled.Events {
		kinds = append(kinds, event.Kind)
		if event.Kind == "decision_awaited" {
			awaited++
		}
		if event.Kind == "decision_recorded" {
			assert.Equal(t, "choose", event.NodeID)
			assert.Equal(t, creator, event.Actor, "the verdict is the human's, not the engine's")
		}
	}
	assert.Equal(t, []string{
		"run_created", "decision_awaited", "decision_recorded",
		"program_prepared", "program_observed", "engine_advanced",
	}, kinds)
	assert.Equal(t, 1, awaited, "the entry obligation is announced exactly once for the run's lifetime")

	duplicate := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "reject"})
	assert.Equal(t, http.StatusConflict, duplicate.Code, duplicate.Body.String())
	assert.Contains(t, duplicate.Body.String(), `"code":"process_decision_stale"`)
}

// TestProcessRuntimeDirectEntryKeepsUnsupportedShapesRefused proves the
// relaxation is only about the entry node's kind: an authoring-valid template
// the engine still cannot execute is refused before a run exists, with the
// diagnostic pointing at the offending node.
func TestProcessRuntimeDirectEntryKeepsUnsupportedShapesRefused(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	tmpl := processDirectParallelEntryTemplate("", 2)
	join := tmpl.Nodes["join"]
	join.Retry = &model.RetryPolicy{MaxAttempts: 2}
	tmpl.Nodes["join"] = join

	createdTemplate := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/templates", processEditResponse{Template: tmpl})
	require.Equal(t, http.StatusCreated, createdTemplate.Code, createdTemplate.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, createdTemplate, &created)

	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": created.ID, "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusUnprocessableEntity, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), `"code":"process_run_invalid"`)
	assert.Contains(t, refused.Body.String(), "nodes.join.retry")
	assert.Contains(t, refused.Body.String(), "retries and poison handling are not executable in this engine yet")
	assert.NotContains(t, refused.Body.String(), "exclusive-decision")

	list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var listed struct {
		Runs []processRuntimeRunView `json:"runs"`
	}
	testharness.DecodeJSON(t, list, &listed)
	assert.Empty(t, listed.Runs, "an ineligible template must not create a run")
}

func processRunEvents(t *testing.T, f *testharness.Flow, runID string) processRuntimeEventPage {
	t.Helper()
	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID+"/events", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	return page
}

func processRunEventKinds(t *testing.T, f *testharness.Flow, runID string) []string {
	t.Helper()
	page := processRunEvents(t, f, runID)
	kinds := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}
