package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserTeamEnvironmentCopiesEditsClearsAndReopens(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, Environment: model.Environment{"PROFILE_VALUE": "original"}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "env", "id": "env", "revision_id": "one", "name": "With variables", "desired": desired}, nil))
	empty := desired
	empty.Environment = nil
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "empty", "id": "empty", "revision_id": "two", "name": "No variables", "desired": empty}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("With variables · one")
	require.Equal(t, "PROFILE_VALUE", page.MustElement("#team-editor [aria-label='Environment name']").MustProperty("value").Str())
	literal := "edited $(not-executed)\nsecond line"
	page.MustElement("#team-editor [aria-label='Environment value']").MustSelectAllText().MustInput(literal)
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	read := func() {
		saved = app.DefinitionResult{}
		require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	}
	read()
	require.Equal(t, model.Environment{"PROFILE_VALUE": literal}, saved.Revision.Team.Members[0].Desired.Environment)
	page.MustElementR("#team-editor button", "^Close team editor$").MustClick()
	page.MustReload()
	page.MustElement("main:not([inert])")
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Worker$").MustClick()
	require.Equal(t, literal, page.MustElement("#team-editor [aria-label='Environment value']").MustProperty("value").Str())
	// Button-only removals also protect unapplied edits when changing sections.
	page.MustElementR("#team-editor [aria-label='Member launch environment'] button", "^Remove$").MustClick()
	page.MustEval(`()=>{window.discardPrompts=0;window.confirm=()=>{window.discardPrompts++;return false}}`)
	page.MustElementR("#team-editor nav button", "^Waves$").MustClick()
	require.Equal(t, 1, page.MustEval(`()=>window.discardPrompts`).Int())
	page.MustElement("#team-editor [name=key]")
	// Copying a profile with no environment clears the draft rather than merging old variables.
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("With variables · one")
	require.Len(t, page.MustElements("#team-editor [aria-label='Environment name']"), 1)
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("No variables · two")
	require.Empty(t, page.MustElements("#team-editor [aria-label='Environment name']"))
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	read()
	require.Empty(t, saved.Revision.Team.Members[0].Desired.Environment)
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/env", nil, &profile))
	require.Equal(t, desired.Environment, profile.Revision.Desired.Environment)
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []any         `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
