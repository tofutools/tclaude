package browser

import (
	"testing"
	"time"

	"github.com/go-rod/rod/lib/input"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserConnectorVerdictCollisionAndReorderedLabels(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	yes := model.WorkEdge{From: "choice", To: "done", Verdict: "yes"}
	no := model.WorkEdge{From: "choice", To: "done", Verdict: "no"}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "wait", Nodes: []model.WorkNode{
		{ID: "wait", Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Duration: time.Second}},
		{ID: "choice", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"yes", "no"}, ExpiresAfter: time.Hour}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "wait", To: "choice"}, yes, no}}
	draft := app.DefinitionDraft{ID: "labels", RevisionID: "v1", Name: "Labels", Source: "test", Kind: model.DefinitionProcess, SchemaVersion: 1, Process: &model.ProcessDefinition{Graph: graph}, EditorLayout: &model.DefinitionEditorLayout{EdgeLabels: []model.EditorEdgeLabel{{Edge: yes, Pinned: false}, {Edge: no, Pinned: true}}}}
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "save", "draft": draft}, &saved))
	page.MustElementR("button", "^Refresh$").MustClick()
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	page.MustElement("#process-editor-canvas .process-edge[data-edge-id$=':no']").MustFocus().MustType(input.Enter)
	page.MustElement("#process-inspector [name=verdict]").MustSelectAllText().MustInput("yes")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor-errors", "That connection already exists")
	require.Equal(t, "yes", page.MustElement("#process-inspector [name=verdict]").MustProperty("value").Str())
	// Correct the retained edit, then close the unchanged saved revision.
	page.MustElement("#process-inspector [name=verdict]").MustSelectAllText().MustInput("no")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	// Remove an unrelated leading edge/node and reverse the remaining edge order.
	// The public revision still binds each label to its exact tuple, not index.
	draft.RevisionID = "v2"
	draft.Process.Graph.EntryNodeID = "choice"
	draft.Process.Graph.Nodes = draft.Process.Graph.Nodes[1:]
	draft.Process.Graph.Edges = []model.WorkEdge{no, yes}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "reorder", "draft": draft, "expected_revision": saved.Definition.Revision}, &saved))
	page.MustElementR("button", "^Refresh$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	require.Empty(t, page.MustElements("#process-editor-canvas .process-edge[data-edge-id$=':yes'] .process-edge-label"))
	require.Equal(t, "no", page.MustElement("#process-editor-canvas .process-edge[data-edge-id$=':no'] .process-edge-label").MustProperty("textContent").Str())
	page.MustElement("#process-editor-canvas .process-edge[data-edge-id$=':yes']").MustFocus().MustType(input.Enter)
	require.Equal(t, "hide", page.MustElement("#process-inspector [name=label_visibility]").MustProperty("value").Str())
	page.MustElement("#process-editor-canvas .process-edge[data-edge-id$=':no']").MustFocus().MustType(input.Enter)
	require.Equal(t, "show", page.MustElement("#process-inspector [name=label_visibility]").MustProperty("value").Str())
}
