package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserEditsGroupMembershipOrderAndBoundedOwner(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team", "members": []string{"alpha", "beta"}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management h3", "Team")
	page.MustElementR("#group-management button", "^Edit name and members$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Updated team")
	page.MustElement("#editor [name=members]").MustSelect("gamma")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management button", "^Change owner$").MustClick()
	page.MustElement("#editor [name=owner]").MustSelect("alpha")
	page.MustElement("#editor [name=harnesses]").MustInput("codex")
	page.MustElement("#editor [name=roots]").MustInput("/tmp")
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var authority app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &authority))
	require.Len(t, authority.Assignments, 1)
	require.Equal(t, []string{"codex"}, authority.Assignments[0].Bounds.Harnesses)
	page.MustElementR("#group-management button", "^Change owner$").MustClick()
	require.Equal(t, "codex", page.MustElement("#editor [name=harnesses]").MustProperty("value").Str())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management .row button", "^Move down$").MustClick()
	var snapshot struct {
		Groups []model.Group `json:"groups"`
	}
	// Wait on the actual refreshed roster before reading the durable ordering.
	page.MustWait(`() => document.querySelector('#group-management .row').textContent.startsWith('beta')`)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Groups, 1)
	require.Equal(t, "Updated team", snapshot.Groups[0].Name)
	require.Equal(t, []model.AgentID{"beta", "alpha", "gamma"}, snapshot.Groups[0].Members)
	require.Equal(t, model.AgentID("alpha"), snapshot.Groups[0].OwnerAgentID)
}
