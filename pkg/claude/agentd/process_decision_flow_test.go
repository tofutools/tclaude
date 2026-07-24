package agentd_test

import (
	"context"
	"net/http"
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

// processDecisionTemplate is an exclusive diamond whose first stop is the
// decision: choose {approve: fast, reject: slow} -> merge -> end.
func processDecisionTemplate(id string) *model.Template {
	task := func(next string) model.Node {
		return model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": next},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"},
		}
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "choose"}},
			"choose": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Approve?"},
				Next:      model.Next{"approve": "fast", "reject": "slow"},
			},
			"fast":  task("merge"),
			"slow":  task("merge"),
			"merge": task("end"),
			"end":   {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

type processDecideResponse struct {
	Started bool                  `json:"started"`
	Run     processRuntimeRunView `json:"run"`
}

func TestProcessRuntimeDecisionLifecycleWithStaleDuplicateAndInvalidInput(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processDecisionTemplate("decision-diamond"))

	completed := make(chan struct{}, 4)
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		result, next, err := executor.Execute(ctx, run, dispatch, authorization)
		completed <- struct{}{}
		return result, next, err
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "decision-diamond", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	require.Equal(t, "awaiting_decision", run.Action)
	require.NotNil(t, run.AwaitingDecision)
	assert.Equal(t, "choose", run.AwaitingDecision.NodeID)
	assert.Equal(t, []string{"approve", "reject"}, run.AwaitingDecision.Verdicts)
	require.NotNil(t, run.Checkpoint.AwaitingDecision())
	assert.Equal(t, engine.NodeReady, run.Checkpoint.Nodes["choose"])
	versionAwaiting := run.StateVersion

	// Resuming an awaiting run is a durable no-op: nothing starts and no new
	// state version or evidence row is minted.
	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, resume.Code, resume.Body.String())
	var resumed processDecideResponse
	testharness.DecodeJSON(t, resume, &resumed)
	assert.False(t, resumed.Started)
	assert.Equal(t, versionAwaiting, resumed.Run.StateVersion)
	assert.Equal(t, "awaiting_decision", resumed.Run.Action)

	invalidVerdict := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "maybe"})
	assert.Equal(t, http.StatusUnprocessableEntity, invalidVerdict.Code, invalidVerdict.Body.String())
	assert.Contains(t, invalidVerdict.Body.String(), `"code":"process_decision_verdict"`)

	wrongNode := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "merge", "verdict": "approve"})
	assert.Equal(t, http.StatusConflict, wrongNode.Code, wrongNode.Body.String())
	assert.Contains(t, wrongNode.Body.String(), `"code":"process_decision_stale"`)

	invalidInput := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve", "evidence": "bad\x00bytes"})
	assert.Equal(t, http.StatusUnprocessableEntity, invalidInput.Code, invalidInput.Body.String())
	assert.Contains(t, invalidInput.Body.String(), `"code":"process_decision_invalid"`)

	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve", "evidence": "intake report checked"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
	var decided processDecideResponse
	testharness.DecodeJSON(t, decide, &decided)
	assert.True(t, decided.Started, "the chosen branch task must start executing")

	<-completed
	<-completed
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Zero(t, agentd.ProcessRunClaimCountForTest())

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var final processRuntimeRunView
	testharness.DecodeJSON(t, show, &final)
	assert.Equal(t, engine.RunCompleted, final.Status)
	assert.Equal(t, "terminal", final.Action)
	assert.Nil(t, final.AwaitingDecision)
	assert.Equal(t, engine.NodeSkipped, final.Checkpoint.Nodes["slow"])
	assert.Equal(t, engine.NodeDone, final.Checkpoint.Nodes["fast"])
	assert.Equal(t, engine.EdgeArrived, final.Checkpoint.Edges["choose"]["approve"])
	assert.Equal(t, engine.EdgeNotTaken, final.Checkpoint.Edges["choose"]["reject"])
	assert.Equal(t, engine.EdgeNotTaken, final.Checkpoint.Edges["slow"]["next"])

	duplicate := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "reject"})
	assert.Equal(t, http.StatusConflict, duplicate.Code, duplicate.Body.String())
	assert.Contains(t, duplicate.Body.String(), `"code":"process_decision_stale"`)

	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID+"/events", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	kinds := make([]string, 0, len(page.Events))
	decisionActor := ""
	decisionPayload := ""
	for _, event := range page.Events {
		kinds = append(kinds, event.Kind)
		if event.Kind == "decision_recorded" {
			decisionActor = event.Actor
			decisionPayload = string(event.Payload)
			assert.Equal(t, "choose", event.NodeID)
		}
	}
	assert.Equal(t, []string{
		"run_created", "decision_awaited", "decision_recorded",
		"program_prepared", "program_observed", "program_prepared", "program_observed", "engine_advanced",
	}, kinds)
	assert.NotEmpty(t, decisionActor, "the decision actor is the authenticated caller")
	assert.Contains(t, decisionPayload, `"verdict":"approve"`)
	assert.Contains(t, decisionPayload, `"evidence":"intake report checked"`)
	assert.Contains(t, decisionPayload, `"chosenEdge":{"from":"choose","outcome":"approve","to":"fast"}`)
}

func TestProcessRuntimeAwaitingDecisionSurvivesRestartAndIsExcludedFromSweep(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processDecisionTemplate("decision-restart")
	record := putProcessRuntimeTemplate(t, root, tmpl)

	// Persist a run already parked on its decision, exactly as a daemon crash
	// after `decision_awaited` would leave it.
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_decision_restart", definition)
	require.NoError(t, err)
	checkpoint, err = engine.AdvanceUntilQuiescent(checkpoint, definition)
	require.NoError(t, err)
	require.NotNil(t, checkpoint.AwaitingDecision())
	createProcessRunFixtureWithCheckpoint(t, "run_decision_restart", record.Ref, tmpl, checkpoint)

	var dispatches atomic.Int32
	completed := make(chan struct{}, 4)
	t.Cleanup(agentd.SetProcessProgramExecuteForTest(func(ctx context.Context, run *executor.Run, dispatch *executor.Dispatch, authorization executor.Authorization) (executor.Result, *executor.Dispatch, error) {
		dispatches.Add(1)
		result, next, err := executor.Execute(ctx, run, dispatch, authorization)
		completed <- struct{}{}
		return result, next, err
	}))

	// The bounded recovery sweep classifies the run in SQLite and never loads
	// or re-prepares it: an awaited decision is not runnable work.
	agentd.RunProcessRunSweepForTest()
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Zero(t, dispatches.Load())

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_decision_restart", nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var view processRuntimeRunView
	testharness.DecodeJSON(t, show, &view)
	assert.Equal(t, "awaiting_decision", view.Action)
	require.NotNil(t, view.AwaitingDecision)
	assert.Equal(t, []string{"approve", "reject"}, view.AwaitingDecision.Verdicts)

	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_decision_restart/decide",
		map[string]any{"nodeId": "choose", "verdict": "reject"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())

	<-completed
	<-completed
	agentd.WaitForProcessRunRuntimeForTest()
	record2, err := db.GetProcessRun("run_decision_restart")
	require.NoError(t, err)
	assert.Equal(t, db.ProcessRunStatusCompleted, record2.Status)
	var final engine.Checkpoint
	require.NoError(t, record2.DecodeCheckpoint(&final))
	assert.Equal(t, engine.NodeDone, final.Nodes["slow"])
	assert.Equal(t, engine.NodeSkipped, final.Nodes["fast"])
}
