package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"testing"
)

func TestBrowserTeamFastModeOverrideAndProfileCopy(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &codex.Provider{})
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalOnRequest, Sandbox: model.SandboxUnconfined, FastMode: model.FastModeOn}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "fast_profile", "id": "worker", "revision_id": "one", "name": "Fast profile", "desired": desired}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=profile]").MustSelect("Fast profile")
	require.Equal(t, "on", page.MustElement("#team-editor [name=fast_mode]").MustProperty("value").Str())
	require.True(t, page.MustElement("#team-editor [name=fast_mode]").MustProperty("disabled").Bool())
	page.MustElement("#team-editor [name=override_fast_mode]").MustClick()
	page.MustElement("#team-editor [name=fast_mode]").MustSelect("Off")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, model.FastModeOff, *saved.Revision.Team.Members[0].Overrides.FastMode)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	require.Equal(t, "off", page.MustElement("#team-editor [name=fast_mode]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_fast_mode]").MustClick()
	require.Equal(t, "on", page.MustElement("#team-editor [name=fast_mode]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=profile]").MustSelect("Custom settings")
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("Fast profile · one")
	require.Equal(t, "on", page.MustElement("#team-editor [name=fast_mode]").MustProperty("value").Str())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].ProfileID)
	require.Equal(t, model.FastModeOn, saved.Revision.Team.Members[0].Desired.FastMode)
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	page.MustElement("#team-editor [name=harness]").MustSelect("claude")
	require.True(t, page.MustElement("#team-editor [name=fast_mode]").MustProperty("disabled").Bool())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 3 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].Desired.FastMode)

}
