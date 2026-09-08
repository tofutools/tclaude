package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserPlanApprovalRetryAuthorsReopensAndClears(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Delivery")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Prepare the delivery")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-inspector button", "^Add plan$").MustClick()
	page.MustElementR("#process-inspector button", "^Require plan approval$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit plan$").MustClick()
	page.MustElement("#process-inspector [name=approval_attempts]").MustInput("9007199254740993")
	page.MustElement("#process-inspector [name=approval_backoff]").MustInput(" 30s ")
	page.MustElement("#process-inspector [name=approval_mode]").MustSelect("feedback-same-session")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Delivery, task']").MustClick()
	page.MustElementR("#process-inspector button", "^Edit plan$").MustClick()
	require.Equal(t, "9007199254740993", page.MustElement("#process-inspector [name=approval_attempts]").MustProperty("value").Str())
	require.Equal(t, " 30s ", page.MustElement("#process-inspector [name=approval_backoff]").MustProperty("value").Str())
	require.Equal(t, "feedback-same-session", page.MustElement("#process-inspector [name=approval_mode]").MustProperty("value").Str())
	page.MustElementR("#process-inspector button", "^Back to task stages$").MustClick()
	page.MustElementR("#process-inspector button", "^Remove plan approval$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 2 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Stages != nil {
			require.NotNil(t, node.Stages.Plan)
			require.Nil(t, node.Stages.PlanApproval)
			require.Nil(t, node.Stages.Plan.ApprovalRetry)
		}
	}
}
