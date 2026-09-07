package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserRosterHierarchyOrderAndDirectMemberFiltering(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	for _, id := range []string{"parent", "child_a", "child_b"} {
		members := []string{}
		if id == "child_a" {
			members = []string{"alpha"}
		}
		if id == "child_b" {
			members = []string{"beta"}
		}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": id, "name": "Team", "members": members}, nil))
	}
	for _, id := range []string{"child_a", "child_b"} {
		require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/"+id+"/parent", map[string]any{"request_id": "nest_" + id, "parent_group_id": "parent", "expected_revision": 1}, nil))
	}
	page.MustElement("#refresh").MustClick()
	page.MustElement("#roster [data-group-id=parent] > .roster-children > [data-group-id=child_b]")
	order := `() => [...document.querySelectorAll('#roster [data-group-id=parent] > .roster-children > [data-group-id]')].map(e=>e.dataset.groupId).join(',')`
	require.Equal(t, "child_a,child_b", page.MustEval(order).Str())
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElement("#group-management [data-group-id=child_b]").MustElementR("button", "^Move group earlier$").MustClick()
	page.MustElementR("#group-order-status", "preferences saved")
	require.Equal(t, "child_b,child_a", page.MustEval(order).Str())
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	require.Equal(t, "child_b,child_a", page.MustEval(order).Str())
	page.MustElement("[aria-label='Search agents']").MustInput("beta")
	page.MustElement("#roster [data-group-id=parent] > .roster-children > [data-group-id=child_b]")
	require.False(t, page.MustHas("#roster [data-group-id=child_a]"))
	page.MustElementR("#roster button", "^Select visible$").MustClick()
	page.MustElementR("#roster", "1 visible · 1 selected")
	page.MustElementR("#roster button", "^Clear selection$").MustClick()
	page.MustElement("[aria-label='Search agents']").MustSelectAllText().MustInput("")
	page.MustElement("[aria-label='Group filter']").MustSelect("Team · parent")
	require.Empty(t, page.MustElements("#roster input[type=checkbox]"), "parent filter must not inherit child membership")
	page.MustElement("[aria-label='Group filter']").MustSelect("Team · child_b")
	page.MustElement("#roster [data-group-id=parent] > .roster-children > [data-group-id=child_b]")
	require.Len(t, page.MustElements("#roster input[type=checkbox]"), 1)
	require.Equal(t, "beta", page.MustElement("#roster .name").MustText())
	var snapshot struct {
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
	for _, g := range snapshot.Groups {
		if g.ID == "parent" {
			require.Empty(t, g.Members)
		} else {
			require.Len(t, g.Members, 1)
			require.Equal(t, model.GroupID("parent"), g.ParentGroupID)
		}
	}
	// An agent may also belong directly to an ancestor. Filtering the child
	// retains that heading, but must not duplicate its selectable row there.
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/parent", map[string]any{"name": "Team", "members": []string{"beta"}, "expected_revision": 1}, nil))
	page.MustEval(`() => {window.oldRosterChild=document.querySelector('#roster [data-group-id=child_b]')}`)
	page.MustElement("#refresh").MustClick()
	page.MustWait(`() => !window.oldRosterChild.isConnected`)
	page.MustElement("#roster [data-group-id=parent] > .roster-children > [data-group-id=child_b]")
	require.Len(t, page.MustElements("#roster input[type=checkbox]"), 1)
	require.Empty(t, page.MustElements("#roster [data-group-id=parent] > .row"))
	require.Len(t, page.MustElements("#roster [data-group-id=child_b] > .row"), 1)

}
