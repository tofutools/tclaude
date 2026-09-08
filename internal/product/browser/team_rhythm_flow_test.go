package browser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserTeamRhythmsAuthorAndReopenWithoutPublishingSchedules(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/automation/rules", map[string]any{"request_id": "library", "id": "library", "revision_id": "library_v1", "name": "Library schedule", "owner": model.AuthoritySubject{Kind: model.AuthorityOperator}, "condition": model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Hour}}, "action": model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "Library message"}}, "policy": model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineSkip, Overlap: model.OverlapForbid, MaxActive: 1, ExpiresAfter: time.Hour, Deadline: time.Hour}}, nil))

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
	page.MustElement("#team-editor [name=rules]").MustSelect("Library schedule · revision 1")
	wait, handle := page.MustHandleDialog()
	done := make(chan struct{})
	go func() { defer close(done); wait(); handle(false, "") }()
	page.MustElementR("#team-editor button", "^Edit Status$").MustClick()
	<-done
	require.Equal(t, "library_v1", page.MustElement("#team-editor [name=rules]").MustProperty("value").Str(), "canceling discard retains the unapplied rule selection")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Status$").MustClick()
	require.Equal(t, "10m", page.MustElement("#team-editor [name=interval]").MustProperty("value").Str())
	require.Equal(t, "Report <progress> literally.", page.MustElement("#team-editor [name=body]").MustProperty("value").Str())
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/team", nil, &saved))
	require.Equal(t, []model.TeamRhythm{{Name: "Status", Interval: "10m", Timezone: "UTC", Subject: "Progress", Body: "Report <progress> literally."}}, saved.Revision.Team.Rhythms)
	var rules []model.AutomationRule
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules", nil, &rules))
	require.Len(t, rules, 1, "saving a template must not create schedules")
	require.Equal(t, model.AutomationRuleID("library"), rules[0].ID)
}
