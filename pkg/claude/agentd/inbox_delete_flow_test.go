package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent (recipient) deletes a single message from its
// inbox via DELETE /v1/messages/{id}. The shared row must vanish
// from BOTH the recipient's inbox listing AND the sender's. Pins the
// production read path: a follow-up GET /v1/inbox no longer surfaces
// the row — same call the inbox-watch TUI polls every 3s.
func TestInboxDelete_RecipientPurgesSharedRow(t *testing.T) {
	f := newFlow(t)

	const senderConv = "snd-aaaa-bbbb-cccc-1111"
	const recipConv = "rcv-aaaa-bbbb-cccc-2222"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", senderConv, "alice")
	f.HaveMember("alpha", recipConv, "bob")

	// Insert the message via the production DB writer to mirror what
	// /v1/messages would store; we exercise the delete endpoint, not
	// the send endpoint.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: senderConv,
		ToConv:   recipConv,
		Subject:  "to be deleted",
		Body:     "payload",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	// Recipient sees one message in their inbox.
	if got := inboxCount(t, f.Mux, recipConv); got != 1 {
		t.Fatalf("pre-delete recipient inbox count = %d, want 1", got)
	}

	// DELETE as recipient — the inbox-watch use case.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), recipConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Recipient inbox no longer surfaces it (production read path).
	if got := inboxCount(t, f.Mux, recipConv); got != 0 {
		t.Errorf("post-delete recipient inbox count = %d, want 0", got)
	}

	// Sender's outgoing list also no longer surfaces it — single shared
	// row, deleted once. Mirrors prune semantics the CLI already exposes.
	rows, err := db.ListAgentMessagesFromConv(senderConv, 100)
	if err != nil {
		t.Fatalf("ListAgentMessagesFromConv: %v", err)
	}
	for _, m := range rows {
		if m.ID == id {
			t.Errorf("sender still sees deleted message id %d", id)
		}
	}

	// Re-deleting the same id returns 404 — observable, not silent.
	r2 := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), recipConv)
	rec2 := testharness.Serve(f.Mux, r2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("re-delete: status=%d body=%s, want 404",
			rec2.Code, rec2.Body.String())
	}
}

// Scenario: a third-party agent (NOT sender, NOT recipient) tries to
// delete someone else's message. Daemon must refuse with 403 — the
// row continues to exist for both legitimate parties.
func TestInboxDelete_ThirdPartyForbidden(t *testing.T) {
	f := newFlow(t)

	const senderConv = "snd-cccc-1111-2222-3333"
	const recipConv = "rcv-cccc-1111-2222-4444"
	const stranger = "xxx-cccc-1111-2222-5555"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", senderConv, "alice")
	f.HaveMember("alpha", recipConv, "bob")

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: senderConv,
		ToConv:   recipConv,
		Subject:  "private",
		Body:     "payload",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), stranger)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("third-party DELETE: status=%d body=%s, want 403",
			rec.Code, rec.Body.String())
	}

	// Row must still exist for both parties.
	got, err := db.GetAgentMessage(id)
	if err != nil || got == nil {
		t.Errorf("row should still exist after third-party 403; got %v err=%v", got, err)
	}
}

// inboxCount uses the same /v1/inbox endpoint the inbox-watch TUI
// polls. Asserting at this surface means we're testing the production
// read path the user actually sees.
func inboxCount(t *testing.T, mux http.Handler, convID string) int {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=100", nil), convID)
	rec := testharness.Serve(mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/inbox: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode inbox: %v body=%s", err, rec.Body.String())
	}
	return len(out)
}

