package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserHumanTaskChoicesPersistAndDecideWithoutSpecialLabelEffects(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Evaluate")
	page.MustElement("#process-inspector [name=ask]").MustInput("Is the work ready?")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Evaluate the work")
	page.MustElement("#process-inspector [name=choices]").MustInput("cancel\nChanges needed")
	page.MustElement("#process-inspector [name=outcomes]").MustInput("pass")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor", "matching pass or fail line")
	require.Equal(t, "cancel\nChanges needed", page.MustElement("#process-inspector [name=choices]").MustProperty("value").Str())
	page.MustElement("#process-inspector [name=outcomes]").MustSelectAllText().MustInput("pass\nfail")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-inspector button", "^Add review$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit review$").MustClick()
	page.MustElement("#process-inspector [name=ask]").MustInput("Approve the review?")
	page.MustElement("#process-inspector [name=prompt]").MustSelectAllText().MustInput("")
	page.MustElement("#process-inspector [name=choices]").MustInput("waive")
	page.MustElement("#process-inspector [name=outcomes]").MustInput("pass")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Evaluate, task']").MustClick()
	require.Equal(t, "Is the work ready?", page.MustElement("#process-inspector [name=ask]").MustProperty("value").Str())
	require.Equal(t, "Evaluate the work", page.MustElement("#process-inspector [name=prompt]").MustProperty("value").Str())
	require.Equal(t, "cancel\nChanges needed", page.MustElement("#process-inspector [name=choices]").MustProperty("value").Str())
	page.MustElementR("#process-inspector button", "^Edit review$").MustClick()
	require.Equal(t, "waive", page.MustElement("#process-inspector [name=choices]").MustProperty("value").Str())
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Start process$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var decisions []app.DecisionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
	require.Len(t, decisions, 1)
	require.Equal(t, "Is the work ready?\n\nEvaluate the work", decisions[0].Window.Question)
	runID := decisions[0].Window.Attempt.RunID
	page.MustElement("[data-tab=decisions]").MustClick()
	for _, answer := range []string{"cancel", "waive"} {
		page.MustElementR("#decision-list button", "^Answer$").MustClick()
		page.MustElement("#editor [name=answer]").MustSelect(answer)
		page.MustElement("#editor [name=reason]").MustInput("Accepted through authored answer")
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustElement("#editor").MustWaitInvisible()
	}
	var result struct {
		Run struct {
			State   model.WorkRunState `json:"state"`
			Outcome model.WorkOutcome  `json:"outcome"`
		}
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/work/"+string(runID), nil, &result))
	require.Equal(t, model.WorkRunSucceeded, result.Run.State)
	require.Equal(t, model.WorkOutcomeVerified, result.Run.Outcome)
}
