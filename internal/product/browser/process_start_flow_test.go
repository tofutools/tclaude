package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserExplicitProcessStartPreservesIdentity(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add start$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Begin here")
	page.MustElement("#process-inspector [name=doc]").MustInput("Literal <start> documentation")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElement("[aria-label='Connect from']").MustSelect("Done")
	page.MustElement("[aria-label='Connect to']").MustSelect("Begin here")
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor-errors", "Start nodes cannot have incoming connections")

	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Begin here, start']").MustClick()
	require.Equal(t, "Literal <start> documentation", page.MustElement("#process-inspector [name=doc]").MustProperty("value").Str())
	var defs []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Len(t, defs, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(defs[0].ID), nil, &saved))
	var found bool
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Kind == model.WorkNodeStart {
			found = true
			require.Equal(t, node.ID, saved.Revision.Process.Graph.EntryNodeID)
			require.Contains(t, saved.Revision.EditorLayout.Nodes, node.ID)
		}
	}
	require.True(t, found)
	require.Len(t, saved.Revision.Process.Graph.Edges, 1)
}
