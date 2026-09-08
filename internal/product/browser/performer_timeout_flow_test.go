package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserPerformerTimeoutSurvivesKindChangeAndClears(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Follow up")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Perform the task")
	page.MustElement("#process-inspector [name=timeout]").MustInput(" 30s ")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector", "timeout execution is unavailable")
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Follow up, task']").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("agent")
	require.Equal(t, " 30s ", page.MustElement("#process-inspector [name=timeout]").MustProperty("value").String())
	page.MustElement("#process-inspector [name=brief]").MustInput("Perform the task")
	page.MustElement("#process-inspector [name=timeout]").MustSelectAllText().MustInput("")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 2 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Performer != nil {
			require.Equal(t, model.PerformerAgent, node.Performer.Kind)
			require.Empty(t, node.Performer.Timeout)
		}
	}
}
