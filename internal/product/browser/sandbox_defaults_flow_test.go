package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserSandboxDefaultsUseNamesAndPersistGroupAssignments(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	for _, profile := range []struct{ ID, Name string }{{"global", "Global rules"}, {"group", "Group rules"}} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": profile.ID, "id": profile.ID, "name": profile.Name, "policy": model.SandboxPolicy{}}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustElementR("#sandbox-profiles button", "^Global sandbox profile$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=sandbox_default]").MustSelect("Global rules")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Sandbox profile$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=sandbox_default]").MustSelect("Group rules")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var defaults model.SandboxDefaults
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-defaults", nil, &defaults))
	require.Equal(t, model.SandboxProfileID("global"), defaults.Global)
	require.Equal(t, model.SandboxProfileID("group"), defaults.Groups["team"])
	page.MustReload().MustWaitLoad()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Sandbox profile$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "group", page.MustElement("#editor [name=sandbox_default]").MustProperty("value").Str())
	page.MustElement("#editor [name=sandbox_default]").MustSelect("No group profile (global default still applies)")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	defaults = model.SandboxDefaults{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-defaults", nil, &defaults))
	require.Equal(t, model.SandboxProfileID("global"), defaults.Global)
	require.Empty(t, defaults.Groups)
	var snapshot struct {
		Executions []any `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions, "editing defaults does not launch or restart agents")
}
