package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserArchivesAndRestoresTemplateWithoutChangingItsRevision(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	draft := app.DefinitionDraft{ID: "library", Name: "Reusable template", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "literal authored source", Process: &model.ProcessDefinition{Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: "done", Nodes: []model.WorkNode{{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}}}}
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "setup", "draft": draft}, &saved))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("[data-definition=library] code", "^library$")
	page.MustElementR("[data-definition=library] button", "^Archive template$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("library")
	page.MustEval(`()=>{const fetch=window.fetch.bind(window);window.fetch=async(...args)=>{const response=await fetch(...args);if(String(args[0]).endsWith('/v2/definitions/library/archive')&&args[1]?.method==='POST'){window.fetch=fetch;throw new Error('simulated archive response loss')}return response}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "simulated archive response loss")
	page.MustWait(`()=>!submitting`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	page.MustWait(`()=>!document.querySelector('[data-definition=library]')`)
	var active []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &active))
	require.Empty(t, active)
	page.MustElement("[aria-label='Template status']").MustSelect("Archived")
	page.MustElementR("[data-definition=library] button", "^Restore template$")
	require.False(t, page.MustHasR("[data-definition=library] button", "^(Edit process|Start process)$"))
	page.MustElementR("[data-definition=library] button", "^Inspect definition$").MustClick()
	page.MustElementR("[data-definition=library] pre", "literal authored source")
	var archived app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/library", nil, &archived))
	require.True(t, archived.Definition.Tombstoned)
	require.Equal(t, saved.Revision, archived.Revision)
	page.MustReload()
	page.MustElementR("#connection", "Updated")
	page.MustElement("main:not([inert])")
	page.MustElement("[aria-label='Template status']").MustSelect("Archived")
	page.MustElementR("[data-definition=library] button", "^Restore template$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("library")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	page.MustElement("[aria-label='Template status']").MustSelect("Active")
	page.MustElementR("[data-definition=library] button", "^Edit process$")
	var restored app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/library", nil, &restored))
	require.False(t, restored.Definition.Tombstoned)
	require.Equal(t, saved.Revision, restored.Revision)
}
