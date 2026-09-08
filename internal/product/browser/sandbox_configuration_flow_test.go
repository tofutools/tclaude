package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserSandboxConfigurationRetainsCopiesChangesAndClearsProfile(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var source app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "source", "id": "source", "name": "Sandbox source", "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate}}, &source))
	var selected model.SandboxSelection
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles/selection", map[string]any{"scopes": []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: source.Revision.Ref}}}, &selected))
	desired := model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, HostSandbox: &selected}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "worker", "name": "Worker", "desired": desired}, nil))
	var later app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "later", "id": "source", "name": "Later sandbox", "expected_revision": source.Profile.Revision, "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Environment: model.Environment{"VALUE": "later"}}}, &later))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster button", "^Configure$").MustClick()
	require.Equal(t, "retained", page.MustElement("#editor [name=host_sandbox]").MustProperty("value").Str())
	page.MustElementR("#editor option[value=retained]", "Keep Later sandbox")
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Renamed worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []any         `json:"executions"`
	}
	snapshot.Agents = nil
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, "Renamed worker", snapshot.Agents[0].Name)
	require.Equal(t, &selected, snapshot.Agents[0].Desired.HostSandbox, "unrelated edit retains the selected profile ID")
	page.MustElementR("#roster button", "^Save settings as configuration$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Independent configuration")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Len(t, profiles, 1)
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &saved))
	require.Equal(t, &selected, saved.Revision.Desired.HostSandbox)
	page.MustElementR("#roster button", "^Configure$").MustClick()
	page.MustElement("#editor [name=host_sandbox]").MustSelect("No additional host sandbox")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	snapshot.Agents = nil
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Nil(t, snapshot.Agents[0].Desired.HostSandbox, "clear is an explicit choice")
	page.MustElementR("#roster button", "^Configure$").MustClick()
	page.MustElement("#editor option[value='profile:source']")
	page.MustElement("#editor [name=host_sandbox]").MustSelect("Later sandbox")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	snapshot.Agents = nil
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Equal(t, model.SandboxProfileRef{ProfileID: source.Profile.ID}, snapshot.Agents[0].Desired.HostSandbox.Scopes[0].Ref)
	require.Empty(t, snapshot.Agents[0].Desired.HostSandbox.PolicyHash)
	require.Empty(t, snapshot.Executions, "editing and copying configurations never starts work")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &saved))
	require.Equal(t, &selected, saved.Revision.Desired.HostSandbox, "later agent edits cannot mutate the saved copy")
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("member")
	page.MustElement("#team-editor [name=name]").MustInput("Member")
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("Independent configuration · " + string(saved.Revision.Ref.RevisionID))
	require.Equal(t, "retained", page.MustElement("#team-editor [name=host_sandbox]").MustProperty("value").Str())
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var team app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &team))
	require.Equal(t, &selected, team.Revision.Team.Members[0].Desired.HostSandbox, "team copy retains the selected configuration revision")
	page.MustElementR("#team-editor button", "^Edit Member$").MustClick()
	page.MustElement("#team-editor [name=host_sandbox]").MustSelect("No additional host sandbox")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	team = app.DefinitionResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &team))
	require.Nil(t, team.Revision.Team.Members[0].Desired.HostSandbox)

}
