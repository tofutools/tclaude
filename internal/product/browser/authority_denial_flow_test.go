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

func TestBrowserExplicitDenialPersistsAndRestoresCurrentGrants(t *testing.T) {
	p := &groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}, endpoints: make(chan string, 2)}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "alpha"}
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/authority/grants/send", map[string]any{"subject": subject, "action": "message.send", "resource": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "beta"}}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start", "target": map[string]any{"agent": map[string]any{"agent_id": "alpha", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	send := func(id string) error {
		return actor.Call(ctx, "POST", "/v2/messages", map[string]any{"request_id": id, "body": "Hello", "to": model.MessageAudience{AgentIDs: []model.AgentID{"beta"}}}, nil)
	}
	require.NoError(t, send("before"))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElementR("#access-list button", "^Deny permission$").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: alpha · alpha")
	page.MustElement("#editor [name=action]").MustSelect("message.send")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#access-list [data-denial] strong", "^message.send$")
	var denied *client.Error
	require.ErrorAs(t, send("denied"), &denied)
	require.Equal(t, 403, denied.Status)
	// The same authenticated caller cannot remove or replace its own denial.
	var state app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
	require.Len(t, state.Denials, 1)
	require.ErrorAs(t, actor.Call(ctx, "DELETE", "/v2/authority/denials/"+string(state.Denials[0].ID), map[string]any{"expected_revision": state.Denials[0].Revision}, nil), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustReload().MustWaitLoad()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#access-list [data-denial] strong", "^message.send$")
	page.MustElementR("#access-list button", "^Remove denial$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, send("restored"))
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
	require.Empty(t, state.Denials)
	require.Len(t, state.Grants, 1)
}
