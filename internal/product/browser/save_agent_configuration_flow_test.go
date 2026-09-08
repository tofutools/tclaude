package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserSavesAgentSettingsAsIndependentConfiguration(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "claude", Model: "fixture", Effort: "high", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	var agent model.Agent
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "source", "name": "Source", "desired": desired, "labels": model.AgentLabels{Role: "writer", Description: "Drafts documentation"}}, &agent))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster button", "^Save settings as configuration$").MustClick()
	page.MustElementR("#editor-title", "^Save configuration$")
	require.Equal(t, "fixture", page.MustElement("#editor [name=model]").MustProperty("value").Str())
	require.Equal(t, "high", page.MustElement("#editor [name=effort]").MustProperty("value").Str())
	// Merely opening or cancelling a captured draft creates nothing.
	page.MustEval(`() => document.querySelector('#editor').close()`)
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Empty(t, profiles)
	page.MustElementR("#roster button", "^Save settings as configuration$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Reusable worker")
	page.MustElement("#editor [name=model]").MustSelectAllText().MustInput("edited-fixture")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Len(t, profiles, 1)
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &saved))
	require.Equal(t, "Reusable worker", saved.Profile.Name)
	require.Equal(t, "edited-fixture", saved.Revision.Desired.Model)
	require.Equal(t, "high", saved.Revision.Desired.Effort)
	require.Equal(t, "Source", saved.Revision.Startup.AgentName)
	require.Equal(t, "writer", saved.Revision.Startup.Role)
	require.Equal(t, "Drafts documentation", saved.Revision.Startup.Description)
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []any         `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, agent, snapshot.Agents[0])
	require.Empty(t, snapshot.Executions)
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list strong", "^Reusable worker$")
}
