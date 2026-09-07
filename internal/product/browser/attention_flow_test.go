package browser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestBrowserAttentionRefreshAndOptInNotifications(t *testing.T) {
	p := &groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}, endpoints: make(chan string, 2)}
	ctx, page, operator := processEditorBrowser(t, p)
	send := func(id, body string) model.Message {
		var result model.Message
		require.NoError(t, operator.Call(ctx, "POST", "/v2/messages", map[string]any{"request_id": id, "subject": "Attention " + id, "body": body, "to": model.MessageAudience{Operator: true}}, &result))
		return result
	}
	old := send("existing", "Previously visible message")
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#attention summary", "1 unread")
	page.MustEval(`() => {window.noticeCalls=[];window.permissionCalls=0;window.closedNotices=0;window.Notification=class {static requestPermission(){window.permissionCalls++;return Promise.resolve('granted')}constructor(title,options){window.noticeCalls.push({title,...options})}close(){window.closedNotices++}}}`)
	page.MustElement("#attention summary").MustClick()
	page.MustElementR("#attention button", "^Enable desktop notifications$").MustClick()
	page.MustElementR("#attention button", "^Disable desktop notifications$")
	require.Equal(t, 1, page.MustEval(`() => window.permissionCalls`).Int())
	require.Equal(t, 0, page.MustEval(`() => window.noticeCalls.length`).Int())
	fresh := send("new", "New unread message")
	page.MustWait(`() => !attention.busy`)
	page.MustEval(`async () => {await attention.poll()}`)
	page.MustElementR("#attention summary", "2 unread")
	require.Equal(t, 1, page.MustEval(`() => window.noticeCalls.length`).Int())
	require.Equal(t, "Attention new", page.MustEval(`() => window.noticeCalls[0].body`).Str())
	page.MustEval(`async () => {await attention.poll()}`)
	require.Equal(t, 1, page.MustEval(`() => window.noticeCalls.length`).Int())
	// Attention only navigates. It never marks a recipient read or approves a decision.
	page.MustElementR("#attention button", "^Open messages$").MustClick()
	var snapshot struct {
		Messages []model.Message `json:"messages"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	for _, m := range snapshot.Messages {
		for _, r := range m.Recipients {
			require.Nil(t, r.ReadAt)
		}
	}
	for _, m := range []model.Message{old, fresh} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/messages/"+string(m.ID)+"/read", map[string]any{"request_id": "read_" + string(m.ID), "operator": true}, nil))
	}
	page.MustEval(`async () => {await attention.poll()}`)
	page.MustElementR("#attention summary", "0 unread")
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"attention_actor", "attention_recipient"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch", "target": map[string]any{"agent": map[string]any{"agent_id": "attention_actor", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	require.NoError(t, actor.Call(ctx, "POST", "/v2/access-requests", map[string]any{"request_id": "ask", "action": "message.send", "resource": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "attention_recipient"}, "reason": "Coordinate", "lifetime_seconds": 120}, nil))
	page.MustEval(`async () => {await attention.poll()}`)
	page.MustElementR("#attention summary", "1 decisions")
	page.MustElementR("#attention button", "^Open decisions$").MustClick()
	page.MustElementR("#decision-list button", "^Decide access$")
	page.MustElement("#logout").MustClick()
	page.MustElementR("#attention [role=status]", "Signed out")
	require.False(t, page.MustEval(`() => attention.running`).Bool())
	require.GreaterOrEqual(t, page.MustEval(`() => window.closedNotices`).Int(), 1)
}
