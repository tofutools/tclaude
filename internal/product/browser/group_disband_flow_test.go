package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserDisbandsGroupWithSharedMembersAndRetainsChildren(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "shared", "name": "Shared member", "desired": model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}, nil))
	for _, id := range []string{"remove", "keep", "child"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": id, "name": id, "members": []string{"shared"}}, nil))
	}
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/child/parent", map[string]any{"request_id": "nest", "parent_group_id": "remove", "expected_revision": 1}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.length===3`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management [data-group-id=remove] > .toolbar button", "^Disband group$").MustClick()
	page.MustElementR("#editor", "Shared member.*shared.*also in")
	page.MustElement("#editor [name=confirm]").MustInput("wrong")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor", "Enter the exact group ID")
	page.MustElement("#editor [name=confirm]").MustSelectAllText().MustInput("remove")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>snapshot.groups?.length===2&&!snapshot.groups.some(g=>g.ID==='remove')`)
	page.MustElement("#group-management > [data-group-id=child]")
	var snapshot struct {
		Groups []model.Group `json:"groups"`
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Groups, 2)
	for _, group := range snapshot.Groups {
		require.Equal(t, []model.AgentID{"shared"}, group.Members)
		require.Empty(t, group.ParentGroupID)
	}
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustWait(`()=>snapshot.groups?.length===2`)
}
