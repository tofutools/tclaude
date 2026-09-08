package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserTeamRhythmsAuthorAndReopenWithoutPublishingSchedules(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "team", "draft": app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Nudge team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor nav button", "^Rhythms$").MustClick()
	page.MustElementR("#team-editor button", "^Add recurring nudge$").MustClick()
	page.MustElement("#team-editor [name=name]").MustInput("Status")
	page.MustElement("#team-editor [name=interval]").MustInput("10m")
	page.MustElement("#team-editor [name=subject]").MustInput("Progress")
	page.MustElement("#team-editor [name=body]").MustInput("Report <progress> literally.")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor nav button", "^Rhythms$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Status$").MustClick()
	require.Equal(t, "10m", page.MustElement("#team-editor [name=interval]").MustProperty("value").Str())
	require.Equal(t, "Report <progress> literally.", page.MustElement("#team-editor [name=body]").MustProperty("value").Str())
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/team", nil, &saved))
	require.Equal(t, []model.TeamRhythm{{Name: "Status", Interval: "10m", Timezone: "UTC", Subject: "Progress", Body: "Report <progress> literally."}}, saved.Revision.Team.Rhythms)
	var rules []model.AutomationRule
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules", nil, &rules))
	require.Empty(t, rules, "saving a template must not create schedules")
}
