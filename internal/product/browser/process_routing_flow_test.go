package browser

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserImportedRouteNamesRemainLiteral(t *testing.T) {
	for _, kind := range []string{"task", "decision"} {
		t.Run(kind, func(t *testing.T) {
			ctx, page, operator := processEditorBrowser(t)
			source := fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: literal-routes
start: begin
nodes:
  begin: {type: start, next: {custom-start: work}}
  work:
    type: %s
    name: Literal route
    performer: {kind: human, ask: Follow the authored route}
    next: {cancel: done}
  done: {type: end}
`, kind)
			page.MustElement("[data-tab=processes]").MustClick()
			page.MustElementR("#definition-list button", "^Import legacy process$").MustClick()
			page.MustElement("#process-import textarea").MustInput(source)
			page.MustElementR("#process-import button", "^Inspect source$").MustClick()
			page.MustElement("#process-import select").MustSelect("Operator")
			if kind == "decision" {
				page.MustElement("#process-import input[aria-label^='Decision timeout']").MustInput("1h")
			}
			page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
			page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
			page.MustElementR("#process-import button", "^Open unsaved copy$").MustClick()
			page.MustElementR("#process-editor button", "^Overview$").MustClick()
			page.MustElementR("#process-inspector", "Decision labels, including cancel and waive")
			page.MustElementR("#process-editor button", "^Save revision$").MustClick()
			page.MustElementR("#process-editor-message", "Revision 1 · saved")
			page.MustElementR("#process-editor button", "^Close editor$").MustClick()
			page.MustElementR("#definition-list button", "^Start process$").MustClick()
			page.MustElement("#editor button[type=submit]").MustClick()
			page.MustElement("#editor").MustWaitInvisible()
			var decisions []app.DecisionResult
			require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
			require.Len(t, decisions, 1)
			id := decisions[0].Window.Attempt.RunID
			page.MustElement("[data-tab=decisions]").MustClick()
			page.MustElementR("#decision-list button", "^Answer$").MustClick()
			answer := "cancel"
			if kind == "task" {
				answer = "complete"
			}
			page.MustElement("#editor [name=answer]").MustSelect(answer)
			page.MustElement("#editor [name=reason]").MustInput("Follow the literal route")
			page.MustElement("#editor button[type=submit]").MustClick()
			page.MustElement("#editor").MustWaitInvisible()
			var result struct {
				Run struct {
					State   model.WorkRunState `json:"state"`
					Outcome model.WorkOutcome  `json:"outcome"`
				}
			}
			require.NoError(t, operator.Call(ctx, "GET", "/v2/work/"+string(id), nil, &result))
			require.Equal(t, model.WorkRunSucceeded, result.Run.State)
			require.Equal(t, model.WorkOutcomeVerified, result.Run.Outcome)
		})
	}
}
