package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserPartialTeamMemberSavesAndReopensWithoutProfile(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=partial]").MustClick()
	page.MustElement("#team-editor [name=approval]").MustSelect("Inherit approval")
	page.MustElement("#team-editor [name=sandbox]").MustSelect("Inherit confinement")
	require.False(t, page.MustElement("#team-editor [name=harness]").MustProperty("required").Bool())
	require.False(t, page.MustElement("#team-editor [name=model]").MustProperty("required").Bool())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, &model.ConfigurationOptions{}, saved.Revision.Team.Members[0].Options)
	require.Empty(t, saved.Revision.Team.Members[0].ProfileID)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	require.True(t, page.MustElement("#team-editor [name=partial]").MustProperty("checked").Bool())
	require.Empty(t, page.MustElement("#team-editor [name=harness]").MustProperty("value").String())
	page.MustElement("#team-editor [name=model]").MustInput("model-only")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.NotNil(t, saved.Revision.Team.Members[0].Options.Model)
	require.Equal(t, "model-only", *saved.Revision.Team.Members[0].Options.Model)
	require.Nil(t, saved.Revision.Team.Members[0].Options.Harness)
}
