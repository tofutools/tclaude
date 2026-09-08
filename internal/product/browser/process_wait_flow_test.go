package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProcessWaitTimestampAndSignalSaveReopenAndClear(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add wait$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Await release")
	page.MustElement("#process-inspector [name=until]").MustInput("2030-09-08T10:30:00+02:00")
	page.MustElement("#process-inspector [name=signal]").MustInput("review <approved>")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector", "cannot start")
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Await release, wait']").MustClick()
	require.Equal(t, "2030-09-08T10:30:00+02:00", page.MustElement("#process-inspector [name=until]").MustProperty("value").String())
	require.Equal(t, "review <approved>", page.MustElement("#process-inspector [name=signal]").MustProperty("value").String())
	page.MustElement("#process-inspector [name=until]").MustSelectAllText().MustInput("")
	page.MustElement("#process-inspector [name=signal]").MustSelectAllText().MustInput("")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 2 · saved")
	var result app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
	for _, node := range result.Revision.Process.Graph.Nodes {
		if node.Kind == model.WorkNodeWait {
			require.Empty(t, node.Wait.Until)
			require.Empty(t, node.Wait.Signal)
			require.Positive(t, node.Wait.Duration)
		}
	}
}
