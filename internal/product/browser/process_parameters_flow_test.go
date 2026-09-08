package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
)

func TestBrowserProcessParametersReachHumanQuestionAsLiteralText(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Review release")
	page.MustElement("#process-inspector [name=ask]").MustInput("Review {{ params.subject }}")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Overview$").MustClick()
	page.MustElement("#process-inspector [name=parameter_syntax]").MustClick()
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-errors", "Input references undeclared parameter subject")
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Add parameter$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustInput("release-key")
	page.MustElement("#process-inspector [name=display_name]").MustInput("Release")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-errors", "Parameter keys must be ASCII identifiers")
	page.MustElementR("#process-inspector button", "^Edit release-key$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("subject")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Review release, task']").MustClick()
	require.Equal(t, "Review {{ params.subject }}", page.MustElement("#process-inspector [name=ask]").MustProperty("value").Str())
	require.Empty(t, page.MustElement("#process-inspector [name=prompt]").MustProperty("value").Str())
	page.MustElementR("#process-editor button", "^Overview$").MustClick()
	require.True(t, page.MustElement("#process-inspector [name=parameter_syntax]").MustProperty("checked").Bool())
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Start process$").MustClick()
	literal := "<img src=x onerror=alert(1)> {{ params.subject }}"
	page.MustElement("#editor [aria-label='Release (subject)']").MustInput(literal)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	var decisions []app.DecisionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
	require.Len(t, decisions, 1)
	require.Equal(t, "Review "+literal, decisions[0].Window.Question)
	page.MustElement("[data-tab=decisions]").MustClick()
	page.MustElementR("#decision-list h2", "Review")
	require.Contains(t, page.MustElement("#decision-list").MustText(), literal)
	require.False(t, page.MustHas("#decision-list img"))
}
