package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
	"testing"
)

func TestBrowserTeamToolGovernanceOverrideAndProfileCopy(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &opencode.Provider{})
	desired := model.DesiredConfiguration{Harness: "opencode", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalDeny, Sandbox: model.SandboxUnconfined, ToolGovernance: model.ToolGovernanceDeny}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "tools_profile", "id": "worker", "revision_id": "one", "name": "Tool profile", "desired": desired}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=profile]").MustSelect("Tool profile")
	require.Equal(t, "deny", page.MustElement("#team-editor [name=tool_governance]").MustProperty("value").Str())
	require.True(t, page.MustElement("#team-editor [name=tool_governance]").MustProperty("disabled").Bool())
	page.MustElement("#team-editor [name=override_tool_governance]").MustClick()
	page.MustElement("#team-editor [name=tool_governance]").MustSelect("Ask before audited tools")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, model.ToolGovernanceAsk, *saved.Revision.Team.Members[0].Overrides.ToolGovernance)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	require.Equal(t, "ask", page.MustElement("#team-editor [name=tool_governance]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_tool_governance]").MustClick()
	require.Equal(t, "deny", page.MustElement("#team-editor [name=tool_governance]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=profile]").MustSelect("Custom settings")
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("Tool profile · one")
	require.Equal(t, "deny", page.MustElement("#team-editor [name=tool_governance]").MustProperty("value").Str())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].ProfileID)
	require.Equal(t, model.ToolGovernanceDeny, saved.Revision.Team.Members[0].Desired.ToolGovernance)
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	page.MustElement("#team-editor [name=harness]").MustSelect("claude")
	require.True(t, page.MustElement("#team-editor [name=tool_governance]").MustProperty("disabled").Bool())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 3 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].Desired.ToolGovernance)

}
