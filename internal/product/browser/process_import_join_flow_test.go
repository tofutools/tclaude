package browser

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserImportsExplicitJoinsWithoutReplacingOriginalNode(t *testing.T) {
	for _, mode := range []string{"all", "any"} {
		t.Run(mode, func(t *testing.T) {
			ctx, page, operator := processEditorBrowser(t)
			source := fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: legacy_join
start: fork
nodes:
  fork:
    type: parallel
    next: {left: merge, right: merge}
  merge:
    type: task
    name: Original reducer
    join: %s
    performer: {kind: human, ask: Review joined work}
    next: done
  done: {type: end}
layout:
  nodes:
    merge: {x: 400, y: 300}
`, mode)
			page.MustElement("[data-tab=processes]").MustClick()
			page.MustElementR("#definition-list button", "^Import legacy process$").MustClick()
			page.MustElement("#process-import textarea").MustInput(source)
			page.MustElementR("#process-import button", "^Inspect source$").MustClick()
			page.MustElement("#process-import select").MustSelect("Operator")
			page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
			page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
			page.MustElementR("#process-import button", "^Open unsaved copy$").MustClick()
			page.MustElementR("#process-editor button", "^Save revision$").MustClick()
			page.MustElementR("#process-editor-message", "Revision 1 · saved")
			var definitions []model.Definition
			require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
			require.Len(t, definitions, 1)
			var result app.DefinitionResult
			require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
			require.Equal(t, source, result.Revision.Source)
			require.Equal(t, model.EditorPosition{X: 400, Y: 300}, result.Revision.EditorLayout.Nodes["merge"])
			count := 0
			for _, n := range result.Revision.Process.Graph.Nodes {
				if n.Kind == model.WorkNodeJoin {
					count++
					require.Equal(t, model.JoinMode(mode), n.Join.Mode)
				}
				if n.ID == "merge" {
					require.Equal(t, "Original reducer", n.Name)
					require.Equal(t, model.WorkNodeTask, n.Kind)
				}
			}
			require.Equal(t, 1, count)
			page.MustElementR("#process-editor button", "^Close editor$").MustClick()
			page.MustElementR("#definition-list button", "^Edit process$").MustClick()
			page.MustElementR("#process-editor button", "^Source$").MustClick()
			require.Equal(t, source, page.MustElement("#process-inspector [name=source]").MustProperty("value").Str())
			page.MustElementR("#process-editor button", "^Close editor$").MustClick()
			page.MustElementR("#definition-list button", "^Start process$").MustClick()
			page.MustElement("#editor button[type=submit]").MustClick()
			page.MustElement("#editor").MustWaitInvisible()
			page.MustWait(`()=>!submitting`)
			var decisions []app.DecisionResult
			require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
			require.Len(t, decisions, 1)
			require.Equal(t, model.WorkNodeID("merge"), decisions[0].Window.Attempt.NodeID)
			require.Equal(t, "Review joined work", decisions[0].Window.Question)
		})
	}
}

func TestBrowserImportsNestedInteriorStartJoinAndCompletes(t *testing.T) {
	_, page, _ := processEditorBrowser(t)
	source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: nested-start-reducer
start: outer
nodes:
  outer:
    type: parallel
    next: {left: inner, right: outer_join}
  inner:
    type: parallel
    next: {a: inner_join, b: inner_join}
  inner_join:
    type: start
    name: Inner control
    join: all
    next: {pass: outer_join}
  outer_join:
    type: end
    join: all
`
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Import legacy process$").MustClick()
	page.MustElement("#process-import textarea").MustInput(source)
	page.MustElementR("#process-import button", "^Inspect source$").MustClick()
	page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
	page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
	page.MustElementR("#process-import button", "^Open unsaved copy$").MustClick()
	page.MustElement("#process-editor-canvas .process-node[aria-label='Inner control, parallel']")
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Start process$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting && snapshot.work_runs?.length===1 && snapshot.work_runs[0].run.state==='succeeded'`)
	require.True(t, page.MustEval(`()=>snapshot.work_runs[0].run.node_attempts.filter(a=>a.Ref.NodeID==='inner_join').length===1`).Bool())
}
