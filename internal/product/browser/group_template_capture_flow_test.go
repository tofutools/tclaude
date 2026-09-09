package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserCapturesGroupAsIndependentUnsavedTeam(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", Effort: "high", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, Environment: model.Environment{"TEAM_SETTING": "literal value"}}
	for _, id := range []string{"second", "first", "retired", "child_only"} {
		memberDesired := desired
		if id == "first" {
			memberDesired.Model = "first-member-model"
		}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": "Same name", "desired": memberDesired}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "source", "name": "Source", "members": []string{"second", "first", "retired"}}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "child", "name": "Child", "members": []string{"child_only"}}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/child/parent", map[string]any{"request_id": "nest", "parent_group_id": "source", "expected_revision": 1}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents/retired/retire", map[string]any{"expected_revision": 1, "reason": "retained"}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/source/details", map[string]any{"expected_revision": 1, "details": map[string]any{"Mission": "Descriptive mission, not a launch instruction"}}, nil))
	page.MustElement("#refresh").MustClick()
	// Open settings after the setup snapshot has finished replacing the roster.
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='source'&&g.Details?.Mission==='Descriptive mission, not a launch instruction')`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	capture := func() {
		page.MustElementR("#group-management [data-group-id=source] > .toolbar button", "^Save group as team template$").MustClick()
		page.MustElementR("#team-editor-status", "New team.*unsaved")
	}
	capture()
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Empty(t, definitions)
	// Cancelling the prefilled editor has no durable effect.
	page.MustEval(`() => {window.confirm=()=>true}`)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Empty(t, definitions)
	capture()
	// The draft is a copy of displayed settings, not a mutable agent/profile reference.
	changed := desired
	changed.Model = "later-source-model"
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/agents/second", map[string]any{"name": "Changed source", "desired": changed, "expected_revision": 1}, nil))
	page.MustElement("#team-editor [aria-label='Team template name']").MustSelectAllText().MustInput("Reusable source")
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var result app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
	require.Equal(t, "Reusable source", result.Definition.Name)
	team := result.Revision.Team
	require.Len(t, team.Members, 2)
	for i, member := range team.Members {
		require.Equal(t, []string{"member_1", "member_2"}[i], member.Key)
		require.Equal(t, "Same name", member.Name)
		expected := desired
		if i == 1 {
			expected.Model = "first-member-model"
		}
		require.Equal(t, expected, member.Desired)
		require.Empty(t, member.Roles)
		require.False(t, member.Owner)
	}
	require.Equal(t, []string{"member_1", "member_2"}, team.Waves[0].MemberKeys)
	require.Equal(t, model.WorkspacePolicyPerMember, team.WorkspacePolicy)
	require.Empty(t, team.Briefings)
	require.Empty(t, team.Automation)
	require.Contains(t, result.Revision.Source, "Descriptive mission, not a launch instruction")
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Groups     []model.Group `json:"groups"`
		Executions []any         `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 4)
	require.Len(t, snapshot.Groups, 2)
	require.Empty(t, snapshot.Executions)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	require.Len(t, page.MustElements("#team-editor-content article"), 2)
}
