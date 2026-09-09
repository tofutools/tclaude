package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserProcessWorkerConfigurationIsIndependent(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Worker profile")
	page.MustElement("#editor [name=model]").MustInput("source-model")
	page.MustElement("#editor [name=cwd]").MustInput("/tmp")
	page.MustElement("#editor [name=effort]").MustInput("low")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`()=>!document.querySelector('#editor').open`)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Worker")
	page.MustElement("#process-inspector [name=brief]").MustInput("Perform the work")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElement("[aria-label='Copy new worker configuration']").MustSelect("Worker profile")
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`()=>!document.querySelector('#editor').open&&!submitting`)
	page.MustElement("#process-inspector [name=model]").MustSelectAllText().MustInput("independent-model")
	page.MustElement("#process-inspector [name=effort]").MustSelectAllText().MustInput("high")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Worker, task']").MustClick()
	require.Equal(t, "independent-model", page.MustElement("#process-inspector [name=model]").MustProperty("value").Str())
	require.Equal(t, "high", page.MustElement("#process-inspector [name=effort]").MustProperty("value").Str())
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	var source app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &source))
	require.NotNil(t, source.Revision.Options)
	require.Equal(t, "source-model", *source.Revision.Options.Model)
	require.Equal(t, "low", *source.Revision.Options.Effort)
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
