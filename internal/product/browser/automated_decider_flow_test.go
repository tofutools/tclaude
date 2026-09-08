package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserAutomatedDecisionPerformerReopens(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add decision$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Choose")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElement("[aria-label='Decision performer']").MustSelect("Agent decision (authoring only)")
	page.MustElementR("#process-inspector button", "^Edit decision performer$").MustClick()
	page.MustElement("#process-inspector [name=brief]").MustInput("Read the evidence and choose")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Choose, decision']").MustClick()
	require.Equal(t, "agent", page.MustElement("[aria-label='Decision performer']").MustProperty("value").Str())
	page.MustElementR("#process-inspector button", "^Edit decision performer$").MustClick()
	require.Equal(t, "Read the evidence and choose", page.MustElement("#process-inspector [name=brief]").MustProperty("value").Str())
	var defs []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Len(t, defs, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(defs[0].ID), nil, &saved))
	for _, n := range saved.Revision.Process.Graph.Nodes {
		if n.Kind == model.WorkNodeDecision {
			require.NotNil(t, n.Decision.Decider)
			require.Nil(t, n.Performer)
		}
	}
}
