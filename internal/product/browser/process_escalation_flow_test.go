package browser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserAuthorsEscalationLoopWithoutStartingWork(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Deliver")
	page.MustElement("#process-inspector [name=prompt]").MustInput("Deliver change")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Add review$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Add decision$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Escalate")
	page.MustElement("#process-inspector [name=answers]").MustSelectAllText().MustInput("retry\ncancel")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Add end$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Cancelled")
	page.MustElement("#process-inspector [name=outcome]").MustSelect("cancelled")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	connect := func(from, to, verdict string) {
		page.MustElement("[aria-label='Connect from']").MustSelect(from)
		page.MustElement("[aria-label='Connect to']").MustSelect(to)
		page.MustElement("[aria-label='Connection answer or outcome']").MustSelectAllText().MustInput(verdict)
		page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	}
	connect("Deliver", "Escalate", "fail")
	connect("Escalate", "Deliver", "retry")
	connect("Escalate", "Cancelled", "cancel")
	require.Len(t, page.MustElements("#process-editor-canvas .process-edge-back"), 1)
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Len(t, saved.Revision.Process.Graph.Edges, 4)
	require.Empty(t, saved.Revision.Process.Graph.EscalationRetries)
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	require.Len(t, page.MustElements("#process-editor-canvas .process-edge-back"), 1)
	page.MustEval(`() => {const original=URL.createObjectURL;URL.createObjectURL=blob=>{blob.text().then(text=>window.escalationExport=JSON.parse(text).draft);return original(blob)}}`)
	page.MustElementR("#process-editor button", "^Export$").MustClick()
	page.MustWait(`() => !!window.escalationExport`)
	require.True(t, page.MustEval(`() => window.escalationExport.Process.Graph.Edges.some(e=>e.Verdict==='retry')`).Bool())

	require.True(t, page.MustEval(`async () => {
  const {processEscalations}=await import('/process-escalation.js');
  const graph=structuredClone(window.escalationExport.Process.Graph);
  return [{},{Subject:{Kind:'agent',AgentID:'bad id'}},{Subject:{Kind:'execution',ExecutionID:'bad id'}},{RoleID:'bad id'},{RoleID:'role',GroupID:'bad id'}].every(entry=>{
    graph.Nodes.find(n=>n.Kind==='decision').Decision.Audience=[entry];
    const result=processEscalations(graph);
    return result.errors.length>0 && result.retries.size===0;
  });
 }`).Bool(), "an empty audience cannot acquire an exempt return edge")
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	err := operator.Call(ctx, "POST", "/v2/processes", map[string]any{"request_id": "start", "id": "refused-loop", "start": model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}}, nil)
	require.ErrorContains(t, err, "unsupported")
}
