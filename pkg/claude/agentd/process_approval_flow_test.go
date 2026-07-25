package agentd_test

import (
	"context"
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

// processApprovalTemplate is start -> build -> end where build is a compound
// whose plan needs a human approval. Every stage program succeeds, so a person
// at the gate is the only thing that can ever hold the run.
func processApprovalTemplate(id string) *model.Template {
	performer := model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"}
	doPerformer := performer
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "build"}},
			"build": {
				Type:      model.NodeTypeTask,
				Performer: &doPerformer,
				Plan:      &model.Step{ID: "plan", Performer: performer, Approval: model.PlanApprovalHuman},
				Next:      model.Next{"next": "end"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// TestProcessRuntimeRecurringPlanApprovalOverTheDecideEndpoint drives one whole
// human approval loop over the SAME endpoint, permission, CAS, and error family
// an authored decision uses: the window opens, a stale window is refused, a
// rework re-runs the plan, and approving the reopened window lets the run finish.
func TestProcessRuntimeRecurringPlanApprovalOverTheDecideEndpoint(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processApprovalTemplate("plan-approval"))

	completed := make(chan struct{}, 8)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		result, err := executor.Perform(ctx, dispatch)
		completed <- struct{}{}
		return result, err
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "plan-approval", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)

	// The plan program runs on its own; the window opens when it succeeds.
	<-completed
	agentd.WaitForProcessRunRuntimeForTest()

	awaiting := showProcessApprovalRun(t, f, run.ID)
	require.Equal(t, "awaiting_decision", awaiting.Action)
	require.NotNil(t, awaiting.AwaitingDecision)
	assert.Equal(t, "build.plan.approval", awaiting.AwaitingDecision.NodeID)
	assert.Equal(t, []string{engine.ApprovalApprove, engine.ApprovalRework}, awaiting.AwaitingDecision.Verdicts)
	assert.Equal(t, 1, awaiting.AwaitingDecision.Attempt, "a recurring window is addressable")
	assert.Equal(t, engine.NodeReady, awaiting.Checkpoint.Nodes["build.plan.approval"])
	version := awaiting.StateVersion

	// A verdict for a window this run is not asking for is refused through the
	// existing stale-decision family, and changes nothing.
	for _, attempt := range []any{0, 2} {
		stale := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
			map[string]any{"nodeId": "build.plan.approval", "verdict": engine.ApprovalApprove, "attempt": attempt})
		require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
		assert.Contains(t, stale.Body.String(), `"code":"process_decision_stale"`)
	}
	invalid := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "build.plan.approval", "verdict": "looks-fine", "attempt": 1})
	require.Equal(t, http.StatusUnprocessableEntity, invalid.Code, invalid.Body.String())
	assert.Contains(t, invalid.Body.String(), `"code":"process_decision_verdict"`)
	assert.Equal(t, version, showProcessApprovalRun(t, f, run.ID).StateVersion,
		"refused input must not bump the durable version")

	// Rework: the plan runs a second time and the window reopens at attempt 2.
	rework := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "build.plan.approval", "verdict": engine.ApprovalRework,
			"attempt": 1, "evidence": "needs a rollback step"})
	require.Equal(t, http.StatusAccepted, rework.Code, rework.Body.String())
	<-completed
	agentd.WaitForProcessRunRuntimeForTest()

	reopened := showProcessApprovalRun(t, f, run.ID)
	require.NotNil(t, reopened.AwaitingDecision)
	assert.Equal(t, 2, reopened.AwaitingDecision.Attempt)
	assert.Equal(t, 2, reopened.Checkpoint.Attempts["build.plan"], "one rework buys one more plan run")
	assert.Empty(t, reopened.Checkpoint.AttemptCeilings, "a human rework writes no ceiling")

	// The verdict a person formed against the closed window is still refused.
	late := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "build.plan.approval", "verdict": engine.ApprovalApprove, "attempt": 1})
	require.Equal(t, http.StatusConflict, late.Code, late.Body.String())

	approve := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "build.plan.approval", "verdict": engine.ApprovalApprove, "attempt": 2})
	require.Equal(t, http.StatusAccepted, approve.Code, approve.Body.String())
	<-completed
	agentd.WaitForProcessRunRuntimeForTest()

	finished := showProcessApprovalRun(t, f, run.ID)
	assert.Equal(t, engine.RunCompleted, finished.Status)
	assert.Nil(t, finished.AwaitingDecision)
	assert.Equal(t, 1, finished.Checkpoint.Attempts["build.do"])

	kinds := processRunEventKinds(t, f, run.ID)
	assert.Equal(t, 2, countProcessEventKind(kinds, "decision_awaited"), "kinds = %v", kinds)
	assert.Equal(t, 2, countProcessEventKind(kinds, "decision_recorded"), "kinds = %v", kinds)
	assert.Equal(t, 1, countProcessEventKind(kinds, "stage_reset"), "only the rework reset anything")
}

// TestProcessRuntimeAuthoredDecisionViewOmitsTheWindow is the compatibility
// pin: an authored decision opens exactly once, so its view carries no attempt
// key at all and its existing decide request stays valid without one.
func TestProcessRuntimeAuthoredDecisionViewOmitsTheWindow(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processDecisionTemplate("authored-window"))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "authored-window", "authorizeProgramProfiles": []string{"safe"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	assert.NotContains(t, created.Body.String(), `"attempt"`,
		"an authored decision view must stay byte-compatible")

	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	require.NotNil(t, run.AwaitingDecision)
	assert.Zero(t, run.AwaitingDecision.Attempt)

	// Naming a window an authored decision never has is refused; omitting it
	// decides exactly as it always did.
	stale := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve", "attempt": 1})
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	decided := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusAccepted, decided.Code, decided.Body.String())
}

func showProcessApprovalRun(t *testing.T, f *testharness.Flow, runID string) processRuntimeRunView {
	t.Helper()
	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, show, &run)
	return run
}

func countProcessEventKind(kinds []string, kind string) int {
	count := 0
	for _, candidate := range kinds {
		if candidate == kind {
			count++
		}
	}
	return count
}

// TestProcessRuntimeApprovalViewFailsClosedWithoutARecordedPlanAttempt is the
// read-surface half of the same fail-closed rule.
//
// The view decodes the stored checkpoint directly rather than through the
// engine's load validator, so it is its own boundary. Omitting the window for a
// gate that has none would be actively misleading: an absent attempt is how an
// authored decision says "there is no window", so a client would form exactly
// the no-window verdict the run refuses.
func TestProcessRuntimeApprovalViewFailsClosedWithoutARecordedPlanAttempt(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processApprovalTemplate("approval-no-attempt")
	record := putProcessRuntimeTemplate(t, root, tmpl)

	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_approval_no_attempt", definition)
	require.NoError(t, err)
	// The state a plan observation would have produced, minus the one counter
	// that gives the window its identity.
	checkpoint.Nodes["start"] = engine.NodeDone
	checkpoint.Nodes["build"] = engine.NodeRunning
	checkpoint.Nodes["build.plan"] = engine.NodeDone
	checkpoint.Nodes["build.plan.approval"] = engine.NodeReady
	checkpoint.Edges["start"]["next"] = engine.EdgeArrived
	checkpoint.AwaitingDecisions = []engine.DecisionObligation{{NodeID: "build.plan.approval"}}
	createProcessRunFixtureWithCheckpoint(t, "run_approval_no_attempt", record.Ref, tmpl, checkpoint)

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_approval_no_attempt", nil)
	require.Equal(t, http.StatusUnprocessableEntity, show.Code, show.Body.String())
	assert.Contains(t, show.Body.String(), `"code":"process_run_invalid"`)
	assert.NotContains(t, show.Body.String(), `"awaitingDecision"`,
		"a window with no identity must not be described at all")

	// The decide endpoint refuses it too, so neither surface can be used to act
	// on a window the run never opened.
	decide := processRuntimeRequest(t, f, http.MethodPost,
		"/v1/process/runs/run_approval_no_attempt/decide",
		map[string]any{"nodeId": "build.plan.approval", "verdict": engine.ApprovalApprove})
	assert.Equal(t, http.StatusUnprocessableEntity, decide.Code, decide.Body.String())
}
