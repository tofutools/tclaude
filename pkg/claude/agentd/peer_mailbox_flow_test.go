package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPeerMailboxReadGrantPagingAndLiveRevocation(t *testing.T) {
	fh := newFedHarness(t)
	seedMailboxes(t, fh.f)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
	}
	grant := func(method, slug, scope string) {
		t.Helper()
		rec := fedHuman(t, fh.f, method, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": scope})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/mailboxes", "/api/mailbox?id=all", "/api/mailbox?id=human"} {
		require.Equal(t, http.StatusForbidden, serve("GET", path, nil).Code, path)
	}
	// Delivery/roster authority cannot expose private node-wide mail.
	grant("POST", agentd.PermMessageDirect, "group=team")
	require.Equal(t, 403, serve("GET", "/api/mailbox?id=all", nil).Code)
	bad := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeMessagesRead, "scope": "group=team"})
	require.Equal(t, 400, bad.Code)
	grant("POST", agentd.PermNodeMessagesRead, "")
	boxes := serve("GET", "/api/mailboxes", nil)
	require.Equal(t, 200, boxes.Code, boxes.Body.String())
	require.Contains(t, boxes.Body.String(), "Human notifications")
	require.Contains(t, boxes.Body.String(), "bob")
	type page struct {
		Messages []struct {
			ID   int64
			Body string
			Read bool
		}
		Page  int `json:"page"`
		Total int `json:"total"`
	}
	var first, second page
	rec := serve("GET", "/api/mailbox?id=all&page_size=1&page=1", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, &first)
	require.Equal(t, 2, first.Total)
	require.Len(t, first.Messages, 1)
	rec = serve("GET", "/api/mailbox?id=all&page_size=1&page=2", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, &second)
	require.Equal(t, 2, second.Page)
	require.Len(t, second.Messages, 1)
	require.Equal(t, "first contact", second.Messages[0].Body)
	require.NotEqual(t, first.Messages[0].ID, second.Messages[0].ID)
	require.False(t, second.Messages[0].Read)
	message, err := db.GetAgentMessage(second.Messages[0].ID)
	require.NoError(t, err)
	require.True(t, message.ReadAt.IsZero(), "reading a peer page must not mark mail read")
	rec = serve("GET", "/api/mailbox?id=human", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "human note one")
	require.Contains(t, rec.Body.String(), "human note two")
	body := map[string]any{"ids": []int64{message.ID}, "read": true}
	require.Equal(t, 403, serve("POST", "/api/mailbox/mark-read", body).Code)
	grant("POST", agentd.PermNodeMessagesManage, "")
	rec = serve("POST", "/api/mailbox/mark-read", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	message, err = db.GetAgentMessage(message.ID)
	require.NoError(t, err)
	require.False(t, message.ReadAt.IsZero())
	// Mark-read authority never implies replies, approvals or destructive cleanup.
	for _, path := range []string{"/api/human-inbox/read", "/api/human-inbox/reply", "/api/human-inbox/access/request", "/api/mailbox/delete", "/api/mailbox/wipe", "/api/human-messages/delete"} {
		require.Equal(t, 403, serve("POST", path, body).Code, path)
	}
	require.Equal(t, 403, serve("GET", "/api/human-inbox", nil).Code)
	grant("DELETE", agentd.PermNodeMessagesRead, "")
	require.Equal(t, 403, serve("GET", "/api/mailboxes", nil).Code)
	require.Equal(t, 403, serve("GET", "/api/mailbox?id=all", nil).Code)
	grant("DELETE", agentd.PermNodeMessagesManage, "")
	require.Equal(t, 403, serve("POST", "/api/mailbox/mark-read", body).Code)
	// Unrestricted still covers normal peer-grantable history permissions.
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	require.Equal(t, 200, serve("GET", "/api/mailbox?id=all", nil).Code)
	// Deleting mail has no peer grant: unrestricted trust reaches it as the
	// local operator does.
	require.NotEqual(t, 403, serve("POST", "/api/mailbox/delete", body).Code)
	// So does deleting human notifications, one or every read one.
	note, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: mbAlice, Body: "delete me"})
	require.NoError(t, err)
	rec = serve("POST", "/api/human-messages/delete", map[string]any{"id": note})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	gone, err := db.GetHumanMessage(note)
	require.True(t, gone == nil || err != nil, "the notification is deleted")
	rec = serve("POST", "/api/human-messages/clear", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

// node.messages.mark-read is the grantable part of a human notification's
// read state: opening one on a peer's page marks it read there, with no
// answer, delete or agent-mail authority. Unrestricted trust holds it.
func TestPeerHumanNotificationMarkReadGrant(t *testing.T) {
	fh := newFedHarness(t)
	seedMailboxes(t, fh.f)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
	}
	grant := func(method, slug string) {
		t.Helper()
		rec := fedHuman(t, fh.f, method, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	id, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: mbAlice, FromTitle: "alice", Body: "open me"})
	require.NoError(t, err)
	isRead := func() bool {
		m, err := db.GetHumanMessage(id)
		require.NoError(t, err)
		return !m.ReadAt.IsZero()
	}
	included := func() []string {
		var out struct {
			PeerView struct {
				Included []string `json:"included"`
			} `json:"peer_view"`
		}
		rec := serve("GET", "/api/mailboxes", nil)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		testharness.DecodeJSON(t, rec, &out)
		return out.PeerView.Included
	}

	grant("POST", agentd.PermNodeMessagesRead)
	require.NotContains(t, included(), "node.messages.mark-read")
	require.Equal(t, 403, serve("POST", "/api/human-inbox/read", map[string]any{"id": id}).Code)
	// Agent mail read state does not reach a human notification's.
	grant("POST", agentd.PermNodeMessagesManage)
	require.Equal(t, 403, serve("POST", "/api/human-inbox/read", map[string]any{"id": id}).Code)
	require.False(t, isRead())

	grant("POST", agentd.PermNodeMessagesMarkRead)
	require.Contains(t, included(), "node.messages.mark-read")
	rec := serve("POST", "/api/human-inbox/read", map[string]any{"id": id})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.True(t, isRead())
	rec = serve("POST", "/api/human-inbox/read", map[string]any{"id": id, "read": false})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.False(t, isRead())
	// Read state only: replies, decisions and the inbox listing stay
	// unrestricted trust's.
	require.Equal(t, 403, serve("POST", "/api/human-inbox/reply", map[string]any{"id": id, "body": "hi"}).Code)
	require.Equal(t, 403, serve("POST", "/api/human-inbox/access/x", map[string]any{"decision": "approve"}).Code)
	require.Equal(t, 403, serve("GET", "/api/human-inbox", nil).Code)

	grant("DELETE", agentd.PermNodeMessagesMarkRead)
	require.Equal(t, 403, serve("POST", "/api/human-inbox/read", map[string]any{"id": id}).Code)

	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	require.Contains(t, included(), "node.messages.mark-read")
	rec = serve("POST", "/api/human-inbox/read", map[string]any{"id": id})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.True(t, isRead())
}
