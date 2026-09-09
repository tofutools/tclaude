package browser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestBrowserGroupMemberCreationGrantEnablesOnlyBoundedGroup(t *testing.T) {
	p := &groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}, endpoints: make(chan string, 2)}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "lead", "name": "Lead", "desired": desired}, nil))
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"id": "worker", "request_id": "profile", "revision_id": "worker_1", "name": "Worker", "desired": desired}, &profile))
	for _, id := range []string{"team", "other"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": id, "name": id}, nil))
		require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/"+id+"/configuration", map[string]any{"profile": profile.Revision.Ref}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch", "target": map[string]any{"agent": map[string]any{"agent_id": "lead", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	create := func(group string) error {
		return actor.Call(ctx, "POST", "/v2/groups/"+group+"/agents", map[string]any{"request_id": "create", "id": "child", "name": "Child", "expected_group_revision": 1, "expected_default_revision": 1}, nil)
	}
	var denied *client.Error
	require.ErrorAs(t, create("team"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.length===2`)
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: Lead · lead")
	page.MustElement("#editor [name=action]").MustSelect("group.members.create")
	page.MustElement("#editor [name=resource]").MustSelect("Group: team · team")
	page.MustElement("#editor [name=configuration]").MustSelect("Only the complete allow-lists below")
	page.MustElement("#editor [name=harnesses]").MustInput(p.Name())
	page.MustElement("#editor [name=models]").MustInput("fixture")
	page.MustElement("#editor [name=roots]").MustInput("/tmp")
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, create("team"))
	require.NoError(t, create("team"))
	require.ErrorAs(t, create("other"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElementR("#access-list button", "^Revoke grant$").MustClick()
	page.MustWait(`()=>document.querySelector('#editor').open`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.ErrorAs(t, create("team"), &denied)
	require.Equal(t, 403, denied.Status)
}
