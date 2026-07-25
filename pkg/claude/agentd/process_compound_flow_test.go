package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// processCompoundTemplate is start -> build -> end where build is a compound
// task. Each stage names a DIFFERENT program profile, which is what makes the
// authorization gate observable: a gate that only looked at the parent's do
// performer would let a run be created with three stages nobody approved.
func processCompoundTemplate(id string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "build"}},
			"build": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "doer", Run: "true"},
				Plan: &model.Step{ID: "plan",
					Performer: model.Performer{Kind: model.PerformerProgram, Profile: "planner", Run: "true"}},
				Checks: []model.Step{{ID: "unit",
					Performer: model.Performer{Kind: model.PerformerProgram, Profile: "checker", Run: "true"}}},
				Review: &model.Step{ID: "review",
					Performer: model.Performer{Kind: model.PerformerProgram, Profile: "reviewer", Run: "true"}},
				Next: model.Next{"next": "end"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// TestProcessRuntimeRefusesCompoundRunMissingStageProgramAuthorization proves
// the authorization boundary covers every program-backed stage performer, and
// that a refusal happens BEFORE a run exists: no run row, no dispatch.
func TestProcessRuntimeRefusesCompoundRunMissingStageProgramAuthorization(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processCompoundTemplate("compound-auth"))
	var dispatched atomic.Int32
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		dispatched.Add(1)
		return executor.Perform(ctx, dispatch)
	}))

	// Authorizing only the parent's do performer is exactly the old gate's idea
	// of complete, and it must now be refused with the stage profiles named.
	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "compound-auth", "authorizeProgramProfiles": []string{"doer"},
	})
	require.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "process_program_unauthorized")
	for _, profile := range []string{"planner", "checker", "reviewer"} {
		assert.Contains(t, refused.Body.String(), profile)
	}
	assert.Zero(t, dispatched.Load())

	list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.Contains(t, list.Body.String(), `"runs":[]`)
}

// TestProcessRuntimeRunsCompoundStagesToCompletionThroughTheProductionMux is the
// end-to-end proof: with every stage profile authorized, the daemon dispatches
// one program per derived stage, in expansion order, and the compound settles
// its authored route into the end node exactly once.
func TestProcessRuntimeRunsCompoundStagesToCompletionThroughTheProductionMux(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processCompoundTemplate("compound-run"))

	completed := make(chan string, 8)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		result, err := executor.Perform(ctx, dispatch)
		completed <- dispatch.Command().NodeID
		return result, err
	}))

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId":               "compound-run",
		"authorizeProgramProfiles": []string{"checker", "doer", "planner", "reviewer"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	require.NotEmpty(t, run.ID)

	dispatchOrder := make([]string, 0, 4)
	for range 4 {
		dispatchOrder = append(dispatchOrder, <-completed)
	}
	agentd.WaitForProcessRunRuntimeForTest()
	assert.Equal(t, []string{"build.plan", "build.do", "build.test.unit", "build.review"}, dispatchOrder)

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	require.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	assert.Equal(t, "terminal", shown.Action)

	// Stages are ordinary checkpoint nodes; the parent and its route settle once.
	for _, nodeID := range []string{
		"start", "build", "build.plan", "build.do", "build.test.unit", "build.review", "build.done", "end",
	} {
		assert.Equal(t, engine.NodeDone, shown.Checkpoint.Nodes[nodeID], nodeID)
	}
	assert.Len(t, shown.Checkpoint.Nodes, 8)
	assert.Equal(t, engine.EdgeArrived, shown.Checkpoint.Edges["build"]["next"])
	// No synthetic stage edges are persisted: only the two authored sources.
	assert.Len(t, shown.Checkpoint.Edges, 2)

	events := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID+"/events", nil)
	require.Equal(t, http.StatusOK, events.Code, events.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, events, &page)
	observed := make([]string, 0, 4)
	for _, event := range page.Events {
		if event.Kind == "program_observed" {
			observed = append(observed, event.NodeID)
		}
	}
	assert.Equal(t, []string{"build.plan", "build.do", "build.test.unit", "build.review"}, observed)
}

// TestProcessRuntimeCompoundColdLoadResumesMidStage restarts the daemon's view
// of the run between stages: the run is reloaded from its persisted template
// snapshot alone, which is the whole point of expanding at prepare time.
func TestProcessRuntimeCompoundColdLoadResumesMidStage(t *testing.T) {
	f, root := processRuntimeFlow(t)
	tmpl := processCompoundTemplate("compound-cold")
	record := putProcessRuntimeTemplate(t, root, tmpl)

	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize("run_compound_cold", definition)
	require.NoError(t, err)
	// Park the fixture exactly where the plan stage has finished and the do stage
	// is next, with nothing about the expansion recorded anywhere.
	checkpoint, err = engine.AdvanceUntilQuiescent(checkpoint, definition)
	require.NoError(t, err)
	require.Equal(t, engine.NodeRunning, checkpoint.Nodes["build"])
	require.Equal(t, engine.NodeReady, checkpoint.Nodes["build.plan"])
	createCompoundRunFixture(t, "run_compound_cold", record.Ref, tmpl, checkpoint)

	completed := make(chan string, 8)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		result, err := executor.Perform(ctx, dispatch)
		completed <- dispatch.Command().NodeID
		return result, err
	}))

	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_compound_cold/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, resume.Code, resume.Body.String())
	for range 4 {
		<-completed
	}
	agentd.WaitForProcessRunRuntimeForTest()

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_compound_cold", nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	assert.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	assert.Equal(t, engine.NodeDone, shown.Checkpoint.Nodes["build.done"])
}

// TestProcessRuntimeRefusesRunsWhoseNodeIDExceedsTheEvidenceEnvelope is the
// regression for the Medium finding of this PR's cold review.
//
// A node id longer than a durable evidence row can name used to be accepted:
// the run was CREATED and then wedged, because every deterministic
// program_prepared transition failed to persist and recovery looped on it. The
// refusal has to happen before the run row exists.
// It covers both halves of the boundary, because neither implies the other: the
// derived case has a parent and a step that are each individually inside the
// ceiling, and the authored case involves no compound at all.
func TestProcessRuntimeRefusesRunsWhoseNodeIDExceedsTheEvidenceEnvelope(t *testing.T) {
	// One byte over, in each of the two ways a node id can get there.
	derived := processCompoundTemplate("compound-long-id")
	build := derived.Nodes["build"]
	build.Checks = []model.Step{{
		ID:        strings.Repeat("c", db.MaxProcessRunNodeIDBytes-len("build.test.")+1),
		Performer: model.Performer{Kind: model.PerformerProgram, Profile: "checker", Run: "true"},
	}}
	derived.Nodes["build"] = build

	longNodeID := strings.Repeat("t", db.MaxProcessRunNodeIDBytes+1)
	authored := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "plain-long-id", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": longNodeID}},
			longNodeID: {
				Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "doer", Run: "true"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}

	for _, test := range []struct {
		name     string
		tmpl     *model.Template
		wantPath string
	}{
		{name: "derived stage id", tmpl: derived, wantPath: "nodes.build.checks[0]"},
		{name: "authored node id", tmpl: authored, wantPath: "nodes." + longNodeID},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			putProcessRuntimeTemplate(t, root, test.tmpl)

			var dispatched atomic.Int32
			t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
				dispatched.Add(1)
				return executor.Perform(ctx, dispatch)
			}))

			refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
				"templateId":               test.tmpl.ID,
				"authorizeProgramProfiles": []string{"checker", "doer", "planner", "reviewer"},
			})
			require.Equal(t, http.StatusUnprocessableEntity, refused.Code, refused.Body.String())
			assert.Contains(t, refused.Body.String(), "process_run_invalid")
			// The eligibility error renders the offending path and the ceiling, so an
			// operator is told what to shorten rather than just "not executable".
			assert.Contains(t, refused.Body.String(), test.wantPath)
			assert.Contains(t, refused.Body.String(), "executable node id ceiling")

			// No run row, and nothing was ever dispatched.
			list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
			require.Equal(t, http.StatusOK, list.Code, list.Body.String())
			assert.Contains(t, list.Body.String(), `"runs":[]`)
			agentd.WaitForProcessRunRuntimeForTest()
			assert.Zero(t, dispatched.Load())
		})
	}
}

// createCompoundRunFixture writes a run row authorized for every stage profile
// this template dispatches. The shared fixture helper authorizes only "safe",
// which a compound template with per-stage profiles could never claim.
func createCompoundRunFixture(t *testing.T, id, ref string, tmpl *model.Template, checkpoint engine.Checkpoint) {
	t.Helper()
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	checkpointJSON, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	require.NoError(t, db.CreateProcessRun(db.ProcessRunCreate{
		ID: id, TemplateRef: ref, TemplateSnapshotJSON: snapshot,
		ParamsJSON:                json.RawMessage(`{}`),
		ProgramAuthorizationsJSON: json.RawMessage(`["checker","doer","planner","reviewer"]`),
		Status:                    string(checkpoint.Status), CheckpointJSON: checkpointJSON,
	}))
}
