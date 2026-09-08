package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserIndependentStageRetryAndModeReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Delivery")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Deliver the work")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-inspector button", "^Add check$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit check 1$").MustClick()
	page.MustElement("#process-inspector [name=attempts]").MustSelectAllText().MustInput("3")
	page.MustElement("#process-inspector [name=backoff]").MustSelectAllText().MustInput("2")
	page.MustElement("#process-inspector [name=retry_mode]").MustSelect("Feedback in same session (authoring only)")
	page.MustElement("#process-inspector [name=retryable]").MustSelect("human_rejected")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Delivery, task']").MustClick()
	page.MustElementR("#process-inspector button", "^Edit check 1$").MustClick()
	require.Equal(t, "3", page.MustElement("#process-inspector [name=attempts]").MustProperty("value").Str())
	require.Equal(t, "2", page.MustElement("#process-inspector [name=backoff]").MustProperty("value").Str())
	require.Equal(t, "feedback-same-session", page.MustElement("#process-inspector [name=retry_mode]").MustProperty("value").Str())
	page.MustElementR("#process-inspector p", "Independent check/review retries")
	var defs []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Len(t, defs, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(defs[0].ID), nil, &saved))
	var found bool
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Stages != nil {
			found = true
			require.Equal(t, uint32(3), node.Stages.Checks[0].Retry.MaxAttempts)
			require.Equal(t, "feedback-same-session", node.Stages.Checks[0].Retry.OnFail)
		}
	}
	require.True(t, found)
}
