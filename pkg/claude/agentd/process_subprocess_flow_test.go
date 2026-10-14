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

// processLintStepTemplate is the referenced template: start -> lint -> end,
// with lint run by its own program profile.
func processLintStepTemplate(run string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "lint-step", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "lint"}},
			"lint": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "linter", Run: run},
				Next:      model.Next{"next": "end"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// processReleaseTemplate runs lint-step by reference, then deploys.
func processReleaseTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "review"}},
			"review": {
				Type:       model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{TemplateRef: "lint-step"},
				Next:       model.Next{"next": "deploy"},
			},
			"deploy": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "deployer", Run: "true"},
				Next:      model.Next{"next": "end"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// Scenario: a template runs another stored template as a subprocess. Creating
// the run needs the inner program's profile authorized too; once it is, the
// daemon runs the referenced body under namespaced ids, then the parent's next
// step, and the run's snapshot keeps the body it pinned even after the
// referenced template changes.
func TestProcessRuntimeRunsReferencedSubprocessToCompletion(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processLintStepTemplate("true"))
	putProcessRuntimeTemplate(t, root, processReleaseTemplate())

	completed := make(chan string, 4)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		result, err := executor.Perform(ctx, dispatch)
		completed <- dispatch.Command().NodeID
		return result, err
	}))

	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "release", "authorizeProgramProfiles": []string{"deployer"},
	})
	require.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "linter", "the inner program's profile must be authorized up front")

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "release", "authorizeProgramProfiles": []string{"deployer", "linter"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)

	// The referenced template changes after the run pinned it; the run keeps
	// executing the version it resolved at creation.
	putProcessRuntimeTemplate(t, root, processLintStepTemplate("false"))

	assert.Equal(t, "review/lint", <-completed)
	assert.Equal(t, "deploy", <-completed)
	agentd.WaitForProcessRunRuntimeForTest()

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	require.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	for _, nodeID := range []string{"review", "review/start", "review/lint", "review/end", "deploy", "end"} {
		assert.Equal(t, engine.NodeDone, shown.Checkpoint.Nodes[nodeID], nodeID)
	}
	assert.Equal(t, engine.EdgeArrived, shown.Checkpoint.Edges["review"]["next"])
	assert.Equal(t, engine.EdgeArrived, shown.Checkpoint.Edges["review/lint"]["next"])
}

// Scenario: a run whose subprocess names a template that does not exist is
// refused at creation, before any run exists.
func TestProcessRuntimeRefusesUnresolvableSubprocessReference(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processReleaseTemplate())

	refused := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "release", "authorizeProgramProfiles": []string{"deployer"},
	})
	assert.NotEqual(t, http.StatusCreated, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "lint-step")

	list := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.Contains(t, list.Body.String(), `"runs":[]`)
}

// Scenario: an inline subprocess parks on a human decision inside its body.
// The decision is addressed by its namespaced id through the ordinary decide
// API, and the chosen out-port routes the parent.
func TestProcessRuntimeDecidesInsideAnInlineSubprocess(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "gated", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "gate"}},
			"gate": {
				Type: model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{
					Start: "check",
					Nodes: map[string]model.Node{
						"check": {
							Type:      model.NodeTypeDecision,
							Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Release?"},
							Next:      model.Next{"yes": "go", "no": "stop"},
						},
						"go":   {Type: model.NodeTypeEnd},
						"stop": {Type: model.NodeTypeEnd},
					},
					Outputs: map[string]string{"go": "approved", "stop": "rejected"},
				},
				Next: model.Next{"approved": "shipped", "rejected": "held"},
			},
			"shipped": {Type: model.NodeTypeEnd, Result: "success"},
			"held":    {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	})

	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{"templateId": "gated"})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var run processRuntimeRunView
	testharness.DecodeJSON(t, created, &run)
	agentd.WaitForProcessRunRuntimeForTest()

	decided := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/"+run.ID+"/decide",
		map[string]any{"nodeId": "gate/check", "verdict": "yes"})
	require.Equal(t, http.StatusAccepted, decided.Code, decided.Body.String())
	agentd.WaitForProcessRunRuntimeForTest()

	show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, show.Code, show.Body.String())
	var shown processRuntimeRunView
	testharness.DecodeJSON(t, show, &shown)
	require.Equal(t, engine.RunCompleted, shown.Checkpoint.Status)
	assert.Equal(t, engine.NodeDone, shown.Checkpoint.Nodes["shipped"])
	assert.Equal(t, engine.NodeSkipped, shown.Checkpoint.Nodes["held"])
	assert.Equal(t, engine.NodeSkipped, shown.Checkpoint.Nodes["gate/stop"])
}
