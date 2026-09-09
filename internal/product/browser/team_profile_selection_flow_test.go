package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
)

func TestBrowserTeamMemberSelectsCurrentNamedProfile(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &copilot.Provider{})
	desired := model.DesiredConfiguration{Harness: "codex", Model: "first", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "worker", "revision_id": "one", "name": "Worker configuration", "desired": desired}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=profile]").MustSelect("Worker configuration")
	require.True(t, page.MustElement("#team-editor [name=model]").MustProperty("disabled").Bool())
	page.MustElement("#team-editor [name=override_model]").MustClick()
	page.MustElement("#team-editor [name=model]").MustSelectAllText().MustInput("member-model")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	require.Contains(t, page.MustElement("#team-editor article").MustText(), "Worker configuration · codex / member-model")
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, model.ConfigurationProfileID("worker"), saved.Revision.Team.Members[0].ProfileID)
	require.True(t, saved.Revision.Team.Members[0].Desired.Equal(model.DesiredConfiguration{}))
	desired.Model = "second"
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile_two", "id": "worker", "revision_id": "two", "expected_revision": 1, "name": "Worker configuration", "desired": desired}, nil))
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	require.Contains(t, page.MustElement("#team-editor article").MustText(), "Worker configuration · codex / member-model")
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	require.Equal(t, "worker", page.MustElement("#team-editor [name=profile]").MustProperty("value").Str())
	require.Equal(t, "member-model", page.MustElement("#team-editor [name=model]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_model]").MustClick()
	require.Equal(t, "second", page.MustElement("#team-editor [name=model]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_harness]").MustClick()
	page.MustElement("#team-editor [name=harness]").MustSelect("copilot")
	page.MustElementR("#team-editor [aria-label='Configured launch support']", "copilot adapter.*Selected policy is supported")
	require.Equal(t, "unconfined", page.MustElement("#team-editor [name=sandbox]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_sandbox]").MustClick()
	page.MustElement("#team-editor [name=sandbox]").MustSelect("workspace_write")
	page.MustElementR("#team-editor [aria-label='Configured launch support']", "Unsupported selected confinement workspace_write")
	require.Equal(t, "workspace_write", page.MustElement("#team-editor [name=sandbox]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=profile]").MustSelect("Custom settings")
	require.False(t, page.MustElement("#team-editor [name=model]").MustProperty("disabled").Bool())
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("Worker configuration · two")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	saved = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Empty(t, saved.Revision.Team.Members[0].ProfileID)
	require.Equal(t, "second", saved.Revision.Team.Members[0].Desired.Model)
}

func TestBrowserTeamPartialProfileRetainsInheritedMemberSettings(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	modelName, fast := "portable-model", model.FastModeOn
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "partial", "id": "partial", "revision_id": "one", "name": "Portable configuration", "options": model.ConfigurationOptions{Model: &modelName, FastMode: &fast}}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=profile]").MustSelect("Portable configuration")
	require.Equal(t, "portable-model", page.MustElement("#team-editor [name=model]").MustProperty("value").Str())
	require.Equal(t, "on", page.MustElement("#team-editor [name=fast_mode]").MustProperty("value").Str())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	require.Contains(t, page.MustElement("#team-editor article").MustText(), "Portable configuration · Inherited harness / portable-model")
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, model.ConfigurationProfileID("partial"), saved.Revision.Team.Members[0].ProfileID)
	require.Nil(t, saved.Revision.Team.Members[0].Overrides)
	require.True(t, saved.Revision.Team.Members[0].Desired.Equal(model.DesiredConfiguration{}))
}
