package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserGroupCapacityPersistsAndRefusesFreshMembership(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"a", "b"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "g", "name": "Group", "members": []string{"a"}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Member limit$").MustClick()
	page.MustElement("#editor [name=limit]").MustSelectAllText().MustInput("1")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management p", "1 active direct members · limit 1")
	require.Error(t, operator.Call(ctx, "PUT", "/v2/groups/g", map[string]any{"name": "Group", "expected_revision": 2, "members": []string{"a", "b"}}, nil))
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management p", "1 active direct members · limit 1")
	page.MustElementR("#group-management button", "^Member limit$").MustClick()
	page.MustElement("#editor [name=limit]").MustSelectAllText().MustInput("0")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/g", map[string]any{"name": "Group", "expected_revision": 3, "members": []string{"a", "b"}}, nil))
	var snapshot struct {
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Groups[0].Members, 2)
	require.Zero(t, snapshot.Groups[0].MaxActiveMembers)
	require.Empty(t, snapshot.Executions)
}
