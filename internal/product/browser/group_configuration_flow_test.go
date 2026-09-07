package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserGroupDefaultsPinMembersAndRetryLostReply(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "first-model", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"first", "second"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "save_" + id, "id": id, "revision_id": "one", "name": "Worker", "desired": desired, "startup": model.ProfileStartup{AgentName: "Suggested", Context: "Pinned context", InitialMessage: "Pinned brief"}}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "group", "name": "Team"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Launch defaults$").MustClick()
	page.MustElement("#editor [name=profile]").MustSelect("Worker · first")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management button", "^Create member from default$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "Suggested", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("First member")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management .row", "First member")
	desired.Model = "changed-model"
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "edit_first", "id": "first", "revision_id": "two", "name": "Worker", "desired": desired, "expected_revision": 1}, nil))
	page.MustEval(`() => {const original=fetch;window.memberCalls=[];let fail=true;window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0])==='/v2/groups/group/agents'){window.memberCalls.push(JSON.parse(args[1].body));if(fail){fail=false;throw new Error('lost group reply')}}return response}}`)
	page.MustElementR("#group-management button", "^Create member from default$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "Suggested", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Second member")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "lost group reply")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.True(t, page.MustEval(`() => memberCalls.length===2 && JSON.stringify(memberCalls[0])===JSON.stringify(memberCalls[1])`).Bool())
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 2)
	require.Len(t, snapshot.Groups[0].Members, 2)
	require.Empty(t, snapshot.Executions)
	for _, a := range snapshot.Agents {
		require.Equal(t, "first-model", a.Desired.Model)
		require.Equal(t, model.ConfigurationProfileID("first"), a.ConfigurationProfile.ProfileID)
		require.Equal(t, model.ConfigurationProfileRevisionID("one"), a.ConfigurationProfile.RevisionID)
	}
	page.MustElementR("#roster button", "^Start with brief$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "Pinned context", page.MustElement("#editor [name=context]").MustProperty("value").Str())
	require.Equal(t, "Pinned brief", page.MustElement("#editor [name=brief]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustElementR("#group-management button", "^Launch defaults$").MustClick()
	page.MustElement("#editor [name=profile]").MustSelect("No group default")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management button", "^Create member from default$").MustClick()
	page.MustElementR("#error", "Choose a group launch default first")
}
