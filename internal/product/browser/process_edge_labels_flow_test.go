package browser

import (
	"encoding/json"
	"testing"

	"github.com/go-rod/rod/lib/input"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProcessEdgeLabelsSaveReopenAndCopy(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add wait$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	editEdge := func() { page.MustElement("#process-editor-canvas .process-edge").MustFocus().MustType(input.Enter) }
	editEdge()
	require.Len(t, page.MustElements("#process-inspector [name=label_visibility]"), 1, page.MustElement("#process-inspector").MustText())
	page.MustElement("#process-inspector [name=label_visibility]").MustSelect("Always show")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	require.Equal(t, "pass", page.MustElement("#process-editor-canvas .process-edge-label").MustProperty("textContent").Str())
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Len(t, saved.Revision.EditorLayout.EdgeLabels, 1)
	require.True(t, saved.Revision.EditorLayout.EdgeLabels[0].Pinned)
	original := saved.Revision.EditorLayout.EdgeLabels[0].Edge
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	editEdge()
	require.Equal(t, "show", page.MustElement("#process-inspector [name=label_visibility]").MustProperty("value").Str())
	page.MustElement("#process-inspector [name=label_visibility]").MustSelect("Hide unless selected")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	require.Empty(t, page.MustElements("#process-editor-canvas .process-edge-label"))
	// Select both real graph nodes with the modifier-preserving keyboard path.
	nodes := page.MustElements("#process-editor-canvas .process-node")
	nodes[0].MustFocus().MustType(input.Enter)
	require.NoError(t, page.Keyboard.Press(input.ShiftLeft))
	page.MustElements("#process-editor-canvas .process-node")[1].MustFocus().MustType(input.Enter)
	require.NoError(t, page.Keyboard.Release(input.ShiftLeft))
	page.MustElementR("#process-inspector h3", "^2 nodes selected$")
	page.MustElementR("#process-editor button", "^Copy nodes$").MustClick()
	page.MustElementR("#process-editor button", "^Saved snippets$").MustClick()
	page.MustElement("#process-snippets [aria-label='New snippet name']").MustInput("Labeled connector")
	page.MustElementR("#process-snippets button", "^Save selected nodes$").MustClick()
	page.MustElementR("#process-snippets h3", "^Labeled connector$")
	var snippets []model.ProcessSnippet
	require.NoError(t, operator.Call(ctx, "GET", "/v2/process-snippets", nil, &snippets))
	require.Len(t, snippets, 1)
	var selection model.ProcessSelection
	require.NoError(t, json.Unmarshal(snippets[0].Selection, &selection))
	require.Len(t, selection.EdgeLabels, 1)
	require.False(t, selection.EdgeLabels[0].Pinned)
	page.MustElementR("#process-snippets button", "^Insert Labeled connector$").MustClick()

	require.Len(t, page.MustElements("#process-editor-canvas .process-node"), 4)
	page.MustEval(`() => {const original=URL.createObjectURL;URL.createObjectURL=blob=>{blob.text().then(text=>window.copiedLabelDraft=JSON.parse(text).draft);return original(blob)}}`)
	page.MustElementR("#process-editor button", "^Export$").MustClick()
	page.MustWait(`() => !!window.copiedLabelDraft`)
	require.True(t, page.MustEval(`() => {const labels=window.copiedLabelDraft.EditorLayout.EdgeLabels;return labels.length===2&&labels.every(l=>l.Pinned===false)&&labels[0].Edge.From!==labels[1].Edge.From&&labels[0].Edge.To!==labels[1].Edge.To}`).Bool())
	// The inserted fragment is not yet connected to the entry. Undo it before
	// saving the executable graph; the independent snippet remains durable.
	page.MustElementR("#process-editor button", "^Undo$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 2 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Len(t, saved.Revision.EditorLayout.EdgeLabels, 1)
	require.False(t, saved.Revision.EditorLayout.EdgeLabels[0].Pinned)
	require.Equal(t, original, saved.Revision.EditorLayout.EdgeLabels[0].Edge)
	// Automatic removes the explicit preference.
	editEdge()
	page.MustElement("#process-inspector [name=label_visibility]").MustSelect("Automatic")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 3 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.EditorLayout.EdgeLabels)
}
