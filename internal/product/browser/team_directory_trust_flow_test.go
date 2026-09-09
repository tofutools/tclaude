package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"testing"
)

func TestBrowserTeamDirectoryTrustOverrideAndProfileCopy(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	desired := model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalManual, Sandbox: model.SandboxWorkspaceWrite, TrustDirectory: true}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "fast_profile", "id": "worker", "revision_id": "one", "name": "Directory trust profile", "desired": desired}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=profile]").MustSelect("Directory trust profile")
	require.Equal(t, "on", page.MustElement("#team-editor [name=trust_directory]").MustProperty("value").Str())
	require.True(t, page.MustElement("#team-editor [name=trust_directory]").MustProperty("disabled").Bool())
	page.MustElement("#team-editor [name=override_trust_directory]").MustClick()
	page.MustElement("#team-editor [name=trust_directory]").MustSelect("Off")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, false, *saved.Revision.Team.Members[0].Overrides.TrustDirectory)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	require.Equal(t, "off", page.MustElement("#team-editor [name=trust_directory]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_trust_directory]").MustClick()
	require.Equal(t, "on", page.MustElement("#team-editor [name=trust_directory]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=profile]").MustSelect("Custom settings")
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("Directory trust profile · one")
	require.Equal(t, "on", page.MustElement("#team-editor [name=trust_directory]").MustProperty("value").Str())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].ProfileID)
	require.Equal(t, true, saved.Revision.Team.Members[0].Desired.TrustDirectory)
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	page.MustElement("#team-editor [name=harness]").MustSelect("codex")
	require.False(t, page.MustElement("#team-editor [name=trust_directory]").MustProperty("disabled").Bool())
	require.Equal(t, "on", page.MustElement("#team-editor [name=trust_directory]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=harness]").MustSelect("opencode")
	require.True(t, page.MustElement("#team-editor [name=trust_directory]").MustProperty("disabled").Bool())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 3 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].Desired.TrustDirectory)

}
