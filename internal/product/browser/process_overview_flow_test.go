package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserProcessOverviewAndParameterHelpKeepExactKeys(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add wait$").MustClick()
	page.MustElement("#process-inspector [name=duration]").MustSelectAllText().MustInput("3600")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Overview$").MustClick()
	page.MustElement("#process-inspector [name=description]").MustInput("Wait for the delivery window")
	page.MustElement("#process-inspector [name=doc]").MustInput("<img src=x onerror=alert(1)>\nLiteral process guide")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Add parameter$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustInput("release_key")
	page.MustElement("#process-inspector [name=display_name]").MustInput("Release label")
	page.MustElement("#process-inspector [name=description]").MustInput("Choose the named release")
	page.MustElement("#process-inspector [name=doc]").MustInput("<b>Literal parameter help</b>")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElementR("#process-editor button", "^Overview$").MustClick()
	require.Equal(t, "Wait for the delivery window", page.MustElement("#process-inspector [name=description]").MustProperty("value").String())
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Start process$").MustClick()
	page.MustElement("#editor [aria-label='Release label (release_key)']").MustInput("release-one")
	require.Contains(t, page.MustElement("#editor-fields").MustText(), "Literal process guide")
	require.Contains(t, page.MustElement("#editor-fields").MustText(), "<b>Literal parameter help</b>")
	require.False(t, page.MustHas("#editor-fields img"))
	require.False(t, page.MustHas("#editor-fields b"))
	page.MustEval(`() => {const original=window.fetch;window.processRequest=null;window.fetch=(url,options)=>{if(url==='/v2/processes')window.processRequest=JSON.parse(options.body);return original(url,options)}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`()=>!document.querySelector('#editor').open`)
	var snapshot struct {
		WorkRuns []struct {
			Run struct {
				ID model.WorkRunID `json:"id"`
			}
		} `json:"work_runs"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.WorkRuns, 1)
	var result struct {
		Run struct {
			Graph model.WorkGraph `json:"graph"`
		}
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/work/"+string(snapshot.WorkRuns[0].Run.ID), nil, &result))
	require.Equal(t, `{"release_key":"release-one"}`, page.MustEval(`()=>JSON.stringify(window.processRequest.start.Parameters)`).String())
	require.Equal(t, "Wait for the delivery window", result.Run.Graph.Description)
	// Saved authoring metadata remains available independently of the run's parameter values.
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, "Release label", saved.Revision.Parameters[0].DisplayName)
	require.Equal(t, "<b>Literal parameter help</b>", saved.Revision.Parameters[0].Doc)
}
