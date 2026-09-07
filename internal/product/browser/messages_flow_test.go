package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserMessageThreadsFilterReadAndExport(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "reader", "name": "Review familiar", "desired": model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}, nil))
	send := func(id, subject, body string, parent model.MessageID, attachment bool) model.Message {
		var result model.Message
		request := map[string]any{"request_id": id, "subject": subject, "body": body, "to": model.MessageAudience{AgentIDs: []model.AgentID{"reader"}}, "cc": model.MessageAudience{Operator: true}, "parent_message_id": parent}
		if attachment {
			request["attachments"] = []app.AttachmentInput{{Filename: "patch.diff", MediaType: "text/plain", Content: []byte("the patch")}}
		}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/messages", request, &result))
		return result
	}
	root := send("root", "Review patch", "Root context", "", true)
	reply := send("reply", "Re: Review patch", "needle reply", root.ID, false)
	other := send("other", "Different topic", "Separate thread", "", false)
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=messages]").MustClick()
	page.MustElementR("#message-list p", "3 matching messages · 2 threads")
	search := page.MustElement("[aria-label='Search messages']")
	search.MustInput("needle")
	page.MustElementR("#message-list p", "1 matching messages · 1 threads")
	require.Len(t, page.MustElements(".message-thread [data-message]"), 2)
	page.MustElementR(".message-thread p", "Thread context")
	page.MustElementR("#message-list button", "^Mark matching operator messages read$").MustClick()
	page.MustElementR("#message-list [role=status]", "Marked 1 messages read")
	var snapshot struct {
		Messages []model.Message `json:"messages"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	for _, m := range snapshot.Messages {
		for _, r := range m.Recipients {
			if m.ID == reply.ID && r.AddressKind == model.MessageAddressOperator {
				require.NotNil(t, r.ReadAt)
			} else {
				require.Nil(t, r.ReadAt, "only the matching operator recipient may be marked read")
			}
		}
	}
	page.MustElement("[aria-label='Message audience']").MustSelect("Operator unread")
	page.MustElementR("#message-list p", "0 matching messages")
	require.True(t, page.MustElementR("#message-list button", "^Mark matching operator messages read$").MustProperty("disabled").Bool())
	search.MustSelectAllText().MustInput("")
	page.MustElement("[aria-label='With attachments']").MustClick()
	page.MustElementR("#message-list p", "1 matching messages · 1 threads")
	page.MustElement(".message-thread summary").MustClick()
	page.MustEval(`() => {window.previousThread=document.querySelector('.message-thread')}`)
	page.MustElement("#refresh").MustClick()
	page.MustWait(`() => document.querySelector('.message-thread') && document.querySelector('.message-thread')!==window.previousThread && !document.querySelector('.message-thread').open`)
	page.MustElement(".message-thread summary").MustClick()
	page.MustEval(`() => {window.exported='';const original=URL.createObjectURL;URL.createObjectURL=function(blob){blob.text().then(t=>window.exported=t);return original(blob)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR(".message-thread button", "^Export thread text$").MustClick()
	page.MustWait(`() => window.exported.includes('needle reply')`)
	exported := page.MustEval(`() => window.exported`).Str()
	require.Contains(t, exported, "Root context")
	require.Contains(t, exported, "patch.diff")
	require.NotContains(t, exported, other.Body)
}
