package browser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProcessStagesAuthorAndReopenOrderedChecks(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElement("[aria-label='Process name']").MustSelectAllText().MustInput("Staged delivery")
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Deliver")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Implement the task")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-inspector button", "^Add plan$").MustClick()
	page.MustElementR("#process-inspector button", "^Require plan approval$").MustClick()
	page.MustElementR("#process-inspector button", "^Add check$").MustClick()
	page.MustElementR("#process-inspector button", "^Add check$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit check 2$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Security check")
	page.MustElement("#process-inspector [name=prompt]").MustSelectAllText().MustInput("Check the security boundary")
	page.MustElement("#process-inspector [name=description]").MustInput("Review external inputs")
	page.MustElement("#process-inspector [name=doc]").MustInput("Preserve literal <policy> examples")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Move check 2 up$").MustClick()
	page.MustElementR("#process-inspector button", "^Add review$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Len(t, saved.Revision.Process.Graph.Nodes, 2, "authored task remains grouped")
	var task model.WorkNode
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Stages != nil {
			task = node
		}
	}
	require.NotNil(t, task.Stages)
	require.True(t, task.Stages.Plan.Performer.Human.Operator)
	require.Equal(t, []string{"approve", "rework"}, task.Stages.PlanApproval.PermittedAnswers)
	require.Len(t, task.Stages.Checks, 2)
	require.Equal(t, "Security check", task.Stages.Checks[0].Name)
	require.Equal(t, "Review external inputs", task.Stages.Checks[0].Description)
	require.Equal(t, "Preserve literal <policy> examples", task.Stages.Checks[0].Doc)
	require.NotNil(t, task.Stages.Review)
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustWait(`() => !document.querySelector("main").inert`)
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Deliver, task']").MustClick()
	page.MustElementR(".process-task-stages", "1. Security check")
	page.MustElementR(".process-task-stages", "Plan requires explicit approval")
	page.MustElementR("#process-inspector button", "^Edit plan$").MustClick()
	page.MustElement("#process-inspector [name=prompt]").MustSelectAllText().MustInput("Revised plan prompt")
	require.True(t, page.MustEval(`() => {const e=new Event('beforeunload',{cancelable:true});window.dispatchEvent(e);return e.defaultPrevented}`).Bool())
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 2 · saved")

	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Start process$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`() => !submitting`)
	var decisions []app.DecisionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
	require.Len(t, decisions, 1)
	runID := decisions[0].Window.Attempt.RunID
	page.MustElement("[data-tab=decisions]").MustClick()
	for _, answer := range []string{"complete", "rework", "complete", "approve", "complete", "reject", "retry", "complete", "complete", "complete", "complete"} {
		page.MustElementR("#decision-list button", "^Answer$").MustClick()
		page.MustElement("#editor").MustWaitVisible()
		options := page.MustEval(`() => [...document.querySelector("#editor [name=answer]").options].map(o=>o.value).join(",")`).Str()

		require.Contains(t, options, answer)
		page.MustElement("#editor [name=answer]").MustSelect(answer)
		page.MustElement("#editor [name=reason]").MustInput("Visible stage feedback: " + answer)
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustElement("#editor").MustWaitInvisible()
		page.MustWait(`() => !submitting`)
	}
	require.Eventually(t, func() bool {
		return operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions) == nil && len(decisions) == 0
	}, 5*time.Second, 20*time.Millisecond)
	var result struct {
		Run struct {
			State    model.WorkRunState      `json:"state"`
			Graph    model.WorkGraph         `json:"graph"`
			Attempts []model.WorkNodeAttempt `json:"node_attempts"`
		} `json:"run"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/work/"+string(runID), nil, &result))
	require.Equal(t, model.WorkRunSucceeded, result.Run.State)
	var retainedFailure bool
	for _, attempt := range result.Run.Attempts {
		if attempt.State == model.NodeAttemptFailed {
			retainedFailure = true
		}
	}
	require.True(t, retainedFailure)
	page.MustElement("[data-tab=work]").MustClick()
	page.MustElementR("#work-list button", "^Inspect process graph$").MustClick()
	page.MustElement(".process-monitor .process-node[data-node-id='" + string(task.ID) + "']").MustClick()
	page.MustElementR(".process-monitor aside", "completion after all stages")
	page.MustElementR(".process-monitor aside h4", "succeeded")
}
