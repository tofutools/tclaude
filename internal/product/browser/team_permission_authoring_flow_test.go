package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserTeamMemberPermissionsEditAndReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)

	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElement("[aria-label='Team template name']").MustSelectAllText().MustInput("Permission team")
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("lead")
	page.MustElement("#team-editor [name=name]").MustInput("Lead")
	page.MustElement("#team-editor [name=harness]").MustSelect("codex")
	page.MustElement("#team-editor [name=model]").MustInput("fixture-model")
	page.MustElementR("#team-editor button", "^Add permission$").MustClick()
	page.MustElementR("#team-editor summary", "^Named constraints$").MustClick()
	page.MustElement("#team-editor [aria-label='Group names (one per line)']").MustInput("work")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	open := func() {
		page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
		page.MustElementR("#team-editor button", "^Edit Lead$").MustClick()
	}
	open()
	require.Equal(t, "group.members.spawn", page.MustElement("#team-editor [aria-label='Permission action']").MustProperty("value").Str())
	page.MustElementR("#team-editor summary", "^Named constraints$").MustClick()
	scope := page.MustElement("#team-editor [aria-label='Group names (one per line)']")
	require.Equal(t, "work", scope.MustProperty("value").Str())
	scope.MustSelectAllText().MustInput("work\nother")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	open()
	page.MustElementR("#team-editor summary", "^Named constraints$").MustClick()
	require.Equal(t, "work\nother", page.MustElement("#team-editor [aria-label='Group names (one per line)']").MustProperty("value").Str())
	page.MustElement("#team-editor [aria-label='Permission effect']").MustSelect("deny")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 3 · saved")
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, []model.TeamMemberPermission{{Action: model.ActionSpawnGroupMember, Denied: true}}, saved.Revision.Team.Members[0].Permissions)
	var authority app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &authority))
	require.Empty(t, authority.Grants)
	require.Empty(t, authority.Denials)
}
