package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserTeamEditorAuthorsRosterWavesBriefingsAndReopens(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElement("[aria-label='Team template name']").MustSelectAllText().MustInput("Review team")
	for _, member := range []struct{ key, name string }{{"builder", "Builder"}, {"reviewer", "Reviewer"}} {
		page.MustElementR("#team-editor button", "^Add member$").MustClick()
		page.MustElement("#team-editor [name=key]").MustInput(member.key)
		page.MustElement("#team-editor [name=name]").MustInput(member.name)
		page.MustElement("#team-editor [name=harness]").MustSelect("codex")
		page.MustElement("#team-editor [name=model]").MustInput("fixture-model")
		page.MustElement("#team-editor [name=cwd]").MustInput("/tmp")
		page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	}
	page.MustElementR("#team-editor nav button", "^Waves$").MustClick()
	page.MustElementR("#team-editor button", "^Add wave$").MustClick()
	page.MustElement("#team-editor [name=id]").MustInput("review")
	page.MustElement("#team-editor [name=members]").MustSelect("Reviewer")
	page.MustElement("#team-editor [name=after]").MustSelect("initial")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor nav button", "^Briefings$").MustClick()
	page.MustElementR("#team-editor button", "^Add briefing$").MustClick()
	page.MustElement("#team-editor [name=id]").MustInput("review-brief")
	page.MustElement("#team-editor [name=body]").MustInput("Review the implementation and report concrete findings.")
	page.MustElement("#team-editor [name=members]").MustSelect("Reviewer")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor nav button", "^Workspace and phases$").MustClick()
	page.MustElement("#team-editor [name=workspace]").MustSelect("Separate member workspaces")
	page.MustElement("#team-editor [name=phases]").MustInput("Implement\nReview")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var result app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
	team := result.Revision.Team
	require.Len(t, team.Members, 2)
	require.Len(t, team.Waves, 2)
	require.Len(t, team.Briefings, 1)
	require.Equal(t, []string{"builder"}, team.Waves[0].MemberKeys)
	require.Equal(t, []string{"initial"}, team.Waves[1].DependsOn)
	require.Equal(t, []string{"reviewer"}, team.Briefings[0].MemberKeys)
	require.Equal(t, model.WorkspacePolicyPerMember, team.WorkspacePolicy)
	require.Equal(t, []string{"Implement", "Review"}, team.AdvisoryPhases)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Reviewer$").MustClick()
	require.Equal(t, "fixture-model", page.MustElement("#team-editor [name=model]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=key]").MustSelectAllText().MustInput("checker")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
	require.Equal(t, []string{"checker"}, result.Revision.Team.Waves[1].MemberKeys)
	require.Equal(t, []string{"checker"}, result.Revision.Team.Briefings[0].MemberKeys)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	require.False(t, page.MustElement("#error").MustVisible())
}
