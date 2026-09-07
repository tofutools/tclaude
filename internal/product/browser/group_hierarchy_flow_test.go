package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserGroupHierarchyUsesStableParentsAndCanUnnest(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	for _, id := range []string{"parent", "child", "other"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": id, "name": "Same label"}, nil))
	}
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management [data-group-id=child] button", "^Move group$").MustClick()
	page.MustElement("#editor [name=parent]").MustSelect("Same label · parent")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("#group-management [data-group-id=parent] > .group-children > [data-group-id=child]")
	var snapshot struct {
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
	var child model.Group
	for _, g := range snapshot.Groups {
		if g.ID == "child" {
			child = g
		}
		require.Empty(t, g.Members)
	}
	require.Equal(t, model.GroupID("parent"), child.ParentGroupID)
	require.Error(t, operator.Call(ctx, "PUT", "/v2/groups/parent/parent", map[string]any{"request_id": "cycle", "parent_group_id": "child", "expected_revision": 1}, nil))
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElement("#group-management [data-group-id=parent] > .group-children > [data-group-id=child]")
	page.MustElementR("#group-management [data-group-id=child] button", "^Move group$").MustClick()
	page.MustElement("#editor [name=parent]").MustSelect("Top level")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("#group-management > [data-group-id=child]")
	snapshot.Groups = nil
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	for _, g := range snapshot.Groups {
		require.Empty(t, g.ParentGroupID)
		require.Empty(t, g.Members)
	}
}
