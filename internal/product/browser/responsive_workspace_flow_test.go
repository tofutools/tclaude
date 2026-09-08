package browser

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserNarrowWorkspaceKeepsNavigationAndSaveReachable(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustSetViewport(390, 740, 1, false)
	require.True(t, page.MustEval(`()=>document.documentElement.scrollWidth<=innerWidth`).Bool(), "workspace must fit the window without horizontal page scrolling")
	page.MustElement("[data-tab=access]").MustClick()
	require.True(t, page.MustEval(`()=>{const r=document.querySelector('[data-tab=access]').getBoundingClientRect();return r.left>=0&&r.right<=innerWidth}`).Bool(), "selected navigation item must be reachable inside the window")
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElement("#new-group").MustClick()
	require.True(t, page.MustEval(`()=>{const r=document.querySelector('#editor').getBoundingClientRect();return r.left>=0&&r.right<=innerWidth}`).Bool(), "save dialog must fit the window")
	page.MustElement("#editor [name=name]").MustInput("Small window group")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct{ Groups []model.Group }
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Groups, 1)
	require.Equal(t, "Small window group", snapshot.Groups[0].Name)
	require.True(t, page.MustEval(`()=>document.documentElement.scrollWidth<=innerWidth`).Bool())
	page.MustElement("#presentation-controls summary").MustClick()
	page.MustElement("#presentation-mode").MustSelect("Wizard")
	page.MustWait(`()=>!presentation.saving && document.body.classList.contains('wizard')`)
	page.MustElement("#presentation-controls summary").MustClick()
	page.MustElement("[data-tab=radio]").MustClick()
	require.True(t, page.MustEval(`()=>document.documentElement.scrollWidth<=innerWidth`).Bool(), "wizard radio controls must fit the window")
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElement("#new-group").MustClick()
	if output := os.Getenv("TCLAUDE_PARITY_SCREENSHOT"); output != "" {
		page.MustScreenshot(output)
	}
	page.MustElement("#cancel").MustClick()
	for _, width := range []int{800, 1440} {
		page.MustSetViewport(width, 900, 1, false)
		page.MustElement("[data-tab=access]").MustClick()
		require.True(t, page.MustEval(`()=>document.documentElement.scrollWidth<=innerWidth`).Bool())
	}

}
