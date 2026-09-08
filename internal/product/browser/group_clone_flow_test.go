package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserClonesGroupAsOfflineIndependentMembers(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "pinned", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "profile", "revision_id": "one", "name": "Worker", "desired": desired}, nil))
	var saved struct {
		Revision struct{ Ref model.ConfigurationProfileRef }
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/profile", nil, &saved))
	for _, id := range []string{"active", "retired"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "configuration_profile": saved.Revision.Ref}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "source", "name": "Source", "members": []string{"active", "retired"}}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/source/owner", map[string]any{"owner_agent_id": "active", "expected_revision": 1, "bounds": map[string]any{}}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents/retired/retire", map[string]any{"expected_revision": 1, "reason": "retained"}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/source/configuration", map[string]any{"profile": saved.Revision.Ref, "expected_revision": 0}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustEval(`() => {const original=fetch;window.cloneCalls=[];let fail=true;window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0])==='/v2/groups/source/clone'){window.cloneCalls.push(JSON.parse(args[1].body));if(fail){fail=false;throw new Error('lost clone reply')}}return response}}`)
	page.MustElementR("#group-management [data-group-id=source] button", "^Clone group$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Independent copy")
	page.MustElement("#editor [name=defaults]").MustSelect("Copy selected configuration Worker and group environment")
	require.Contains(t, page.MustElement("#editor-fields").MustText(), "including later profile edits")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "lost clone reply")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.True(t, page.MustEval(`() => cloneCalls.length===2 && JSON.stringify(cloneCalls[0])===JSON.stringify(cloneCalls[1])`).Bool())
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 3)
	require.Len(t, snapshot.Groups, 2)
	require.Empty(t, snapshot.Executions)
	var cloned model.Group
	for _, group := range snapshot.Groups {
		if group.ID != "source" {
			cloned = group
		}
	}
	require.Equal(t, "Independent copy", cloned.Name)
	require.Empty(t, cloned.OwnerAgentID)
	require.Empty(t, cloned.ParentGroupID)
	require.Len(t, cloned.Members, 1)
	require.NotEqual(t, model.AgentID("active"), cloned.Members[0])
	for _, agent := range snapshot.Agents {
		if agent.ID == cloned.Members[0] {
			require.Equal(t, model.AgentID("active"), agent.CloneSourceAgentID)
			expectedDesired := desired
			expectedDesired.HostSandbox = model.SandboxInGroup(desired.HostSandbox, cloned.ID)
			require.Equal(t, expectedDesired, agent.Desired)
			require.Empty(t, agent.PrimaryExecutionID)
		}
	}
	var defaults model.GroupConfiguration
	require.NoError(t, operator.Call(ctx, "GET", "/v2/groups/"+string(cloned.ID)+"/configuration", nil, &defaults))
	require.Equal(t, &saved.Revision.Ref, defaults.Profile)
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management h3", "^Independent copy$")
	desired.Model = "updated-default"
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "edit_profile", "id": "profile", "revision_id": "two", "expected_revision": 1, "name": "Worker", "desired": desired}, nil))
	page.MustElementR("#group-management [data-group-id="+string(cloned.ID)+"] button", "^Create member from default$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("New from copied default")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 4)
	for _, agent := range snapshot.Agents {
		if agent.Name == "New from copied default" {
			require.Equal(t, "updated-default", agent.Desired.Model)
		} else {
			require.Equal(t, "pinned", agent.Desired.Model)
		}
	}

}

func TestBrowserGroupCloneExplainsArchivedMemberAndAllowsEmptyCopy(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "pinned", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "archived_profile", "revision_id": "one", "name": "Worker", "desired": desired}, nil))
	var saved struct {
		Revision struct{ Ref model.ConfigurationProfileRef }
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/archived_profile", nil, &saved))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "active", "name": "Active member", "configuration_profile": saved.Revision.Ref}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "source", "name": "Source", "members": []string{"active"}}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles/archived_profile/archive", map[string]any{"request_id": "archive", "expected_revision": 1, "archived": true}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management [data-group-id=source] button", "^Clone group$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "none", page.MustElement("#editor [name=members]").MustProperty("value").Str())
	require.False(t, page.MustEval(`() => !!document.querySelector('#editor [name=members] option[value=copy]')`).Bool())
	page.MustElementR("#editor li", "Active member · active.*archived_profile")
	page.MustElementR("#editor p", "Restore the listed archived configurations")
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Empty copy")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Groups, 2)
	require.Empty(t, snapshot.Executions)
	for _, group := range snapshot.Groups {
		if group.ID != "source" {
			require.Empty(t, group.Members)
		}
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles/archived_profile/archive", map[string]any{"request_id": "restore", "expected_revision": 2, "archived": false}, nil))
	page.MustElementR("#group-management [data-group-id=source] button", "^Clone group$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "copy", page.MustElement("#editor [name=members]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
}
