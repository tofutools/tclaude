package browser

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"testing"
)

func TestBrowserUnscopedGrantAuthorsReopensAndRevokes(t *testing.T) {
	p := &groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}, endpoints: make(chan string, 2)}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start", "target": map[string]any{"agent": map[string]any{"agent_id": "alpha", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	send := func(id, target string) error {
		return actor.Call(ctx, "POST", "/v2/messages", map[string]any{"request_id": id, "body": "Hello", "to": model.MessageAudience{AgentIDs: []model.AgentID{model.AgentID(target)}}}, nil)
	}
	var denied *client.Error
	require.ErrorAs(t, send("before", "beta"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: alpha · alpha")
	page.MustElement("#editor [name=action]").MustSelect("message.send")
	page.MustElement("#editor [name=resource]").MustSelect("All resources (this action only)")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#access-list [data-grant] strong", "^message.send$")
	for _, target := range []string{"beta", "gamma"} {
		require.NoError(t, send("allowed_"+target, target))
	}
	var state app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
	require.Len(t, state.Grants, 1)
	require.Equal(t, model.ResourceSelector{Kind: model.ResourceAll}, state.Grants[0].Resource)
	page.MustReload().MustWaitLoad()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#access-list [data-grant] strong", "^message.send$")
	page.MustElementR("#access-list button", "^Edit grant$").MustClick()
	var selected model.ResourceSelector
	require.NoError(t, json.Unmarshal([]byte(page.MustElement("#editor [name=resource]").MustProperty("value").Str()), &selected))
	require.Equal(t, model.ResourceSelector{Kind: model.ResourceAll}, selected)
	page.MustElement("#cancel").MustClick()
	page.MustElementR("#access-list button", "^Revoke grant$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	for _, target := range []string{"beta", "gamma"} {
		require.ErrorAs(t, send("revoked_"+target, target), &denied)
		require.Equal(t, 403, denied.Status)
	}
}
