package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserProcessContactScheduleSurvivesKindChangeAndClears(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Follow up")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Perform the task")
	page.MustElement("#process-inspector [name=contact_cadence]").MustInput("30m")
	page.MustElement("#process-inspector [name=contact_budget]").MustInput("5")
	page.MustElement("#process-inspector [name=contact_target]").MustInput("human:operator")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector", "Running this process is unavailable")
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Follow up, task']").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("agent")
	require.Equal(t, "30m", page.MustElement("#process-inspector [name=contact_cadence]").MustProperty("value").String())
	require.Equal(t, "5", page.MustElement("#process-inspector [name=contact_budget]").MustProperty("value").String())
	require.Equal(t, "human:operator", page.MustElement("#process-inspector [name=contact_target]").MustProperty("value").String())
	page.MustElement("#process-inspector [name=brief]").MustInput("Perform the task")
	for _, field := range []string{"contact_cadence", "contact_budget", "contact_target"} {
		page.MustElement("#process-inspector [name=" + field + "]").MustSelectAllText().MustInput("")
	}
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
			require.Nil(t, node.Performer.Contact)
		}
	}
}
