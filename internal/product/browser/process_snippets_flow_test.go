package browser

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProcessSnippetsReopenInsertUndoAndDelete(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	page.MustElementR("#process-editor button", "^Add wait$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Reusable pause")
	page.MustElement("#process-inspector [name=duration]").MustSelectAllText().MustInput("9007199.254740993")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Saved snippets$").MustClick()
	page.MustElement("#process-snippets [aria-label='New snippet name']").MustInput("Pause fragment")
	page.MustElementR("#process-snippets button", "^Save selected nodes$").MustClick()
	page.MustElementR("#process-snippets h3", "^Pause fragment$")
	var snippets []model.ProcessSnippet
	require.NoError(t, operator.Call(ctx, "GET", "/v2/process-snippets", nil, &snippets))
	require.Len(t, snippets, 1)
	require.True(t, snippets[0].Available)
	var exact model.ProcessSelection
	require.NoError(t, json.Unmarshal(snippets[0].Selection, &exact))
	require.EqualValues(t, 9007199254740993, exact.Nodes[0].Wait.Duration)
	page.MustElementR("#process-snippets button", "^Close snippets$").MustClick()
	// Discard the unsaved editor and reload; the independent library survives.
	page.MustEval(`() => {window.confirm=()=>true}`)
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustWait(`() => !document.querySelector("main").inert`)
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	before := len(page.MustElements("#process-editor-canvas .process-node"))
	page.MustElementR("#process-editor button", "^Saved snippets$").MustClick()
	page.MustElementR("#process-snippets button", "^Insert Pause fragment$").MustClick()
	require.Equal(t, "9007199.254740993", page.MustElement("#process-inspector [name=duration]").MustProperty("value").String())
	page.MustElement("#process-editor-canvas .process-node[aria-label='Reusable pause copy, wait']")
	require.Len(t, page.MustElements("#process-editor-canvas .process-node"), before+1)
	page.MustElementR("#process-editor button", "^Undo$").MustClick()
	require.Len(t, page.MustElements("#process-editor-canvas .process-node"), before)
	// Rename the library item, then delete; neither creates a definition or work.
	page.MustElementR("#process-editor button", "^Saved snippets$").MustClick()
	page.MustElement("#process-snippets [aria-label='Rename Pause fragment']").MustSelectAllText().MustInput("Renamed pause")
	page.MustElementR("#process-snippets button", "^Rename snippet$").MustClick()
	page.MustElementR("#process-snippets h3", "^Renamed pause$")
	page.MustEval(`() => {window.confirm=()=>true}`)
	page.MustElementR("#process-snippets button", "^Delete snippet$").MustClick()
	page.MustElementR("#process-snippets [role=status]", "^0 saved snippets$")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Empty(t, definitions)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/process-snippets", nil, &snippets))
	require.Empty(t, snippets)
	// A pending real authenticated list response cannot recreate a signed-out view.
	page.MustEval(`() => { const real=window.fetch; window.fetch=async(...args)=>{const response=await real(...args);if(String(args[0]).includes('/v2/process-snippets')){await new Promise(resolve=>window.releaseSnippetRead=resolve)}return response}}`)
	page.MustElementR("#process-snippets button", "^Reload snippets$").MustClick()
	page.MustWait(`() => typeof window.releaseSnippetRead==='function'`)
	page.MustEval(`() => {document.dispatchEvent(new Event('workspace-signout'));window.releaseSnippetRead()}`)
	page.MustWait(`() => !document.getElementById('process-snippets')`)
}

func TestBrowserProcessSnippetCanonicalKeysAndZeroLayout(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	selection := json.RawMessage(`{"Version":1,"Nodes":[{"id":"left","kind":"end","name":"Left"},{"id":"right","kind":"end","name":"Right"}],"Edges":[{"from":"left","to":"right"}],"Positions":{"left":{"x":0,"y":0},"right":{"x":200,"y":0}}}`)
	var saved model.ProcessSnippet
	require.NoError(t, operator.Call(ctx, "POST", "/v2/process-snippets/zero", map[string]any{"request_id": "zero", "action": "create", "name": "Zero layout", "selection": selection}, &saved))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	page.MustElementR("#process-editor button", "^Saved snippets$").MustClick()
	page.MustElementR("#process-snippets button", "^Insert Zero layout$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Left copy, end']")
	page.MustElement("#process-editor-canvas .process-node[aria-label='Right copy, end']")
	page.MustEval(`() => {const original=URL.createObjectURL;URL.createObjectURL=blob=>{blob.text().then(text=>window.exportedSnippetDraft=JSON.parse(text).draft);return original(blob)}}`)
	page.MustElementR("#process-editor button", "^Export$").MustClick()
	page.MustWait(`() => !!window.exportedSnippetDraft`)
	require.True(t, page.MustEval(`() => {const d=window.exportedSnippetDraft,g=d.Process.Graph,left=g.Nodes.find(n=>n.Name==='Left copy'),right=g.Nodes.find(n=>n.Name==='Right copy');return left.ID!=='left'&&right.ID!=='right'&&left.ID!==right.ID&&d.EditorLayout.Nodes[left.ID].X===40&&d.EditorLayout.Nodes[left.ID].Y===40&&d.EditorLayout.Nodes[right.ID].X===240&&d.EditorLayout.Nodes[right.ID].Y===40&&g.Edges.some(e=>e.From===left.ID&&e.To===right.ID)}`).Bool())
	page.MustElementR("#process-editor button", "^Undo$").MustClick()
	require.Len(t, page.MustElements("#process-editor-canvas .process-node"), 1)
}
