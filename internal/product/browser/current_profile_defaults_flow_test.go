package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserGlobalDefaultUsesEditedProfileWithoutReselection(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "save", "id": "worker", "revision_id": "one", "name": "Worker", "desired": desired, "startup": model.ProfileStartup{AgentName: "Old suggestion"}}, &saved))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-defaults", map[string]any{"request_id": "default", "global": saved.Revision.Ref}, nil))
	desired.Model = "updated"
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "edit", "id": "worker", "revision_id": "two", "expected_revision": 1, "name": "Worker", "desired": desired, "startup": model.ProfileStartup{AgentName: "Current suggestion", Role: "reviewer"}}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list button", "^Create from global$").MustClick()
	page.MustElementR("#editor-title", "^Create agent from default$")
	require.Equal(t, "Current suggestion", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	require.Equal(t, "reviewer", page.MustElement("#editor [name=role_label]").MustProperty("value").Str())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`()=>!document.querySelector('#editor').open`)
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	require.Equal(t, "updated", snapshot.Agents[0].Desired.Model)
	require.Equal(t, model.ConfigurationProfileRevisionID("two"), snapshot.Agents[0].ConfigurationProfile.RevisionID)
}
