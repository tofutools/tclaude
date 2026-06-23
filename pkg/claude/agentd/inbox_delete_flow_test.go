package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const senderConv = "snd-aaaa-bbbb-cccc-1111"
		const recipConv = "rcv-aaaa-bbbb-cccc-2222"
		g := f.HaveGroup("alpha")
		f.HaveMember("alpha", senderConv)
		f.HaveMember("alpha", recipConv)

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
		require.NoError(t, err, "InsertAgentMessage")

		// Recipient sees one message in their inbox.
		require.Equal(t, 1, inboxCount(t, f.Mux, recipConv), "pre-delete recipient inbox count")

		// DELETE as recipient — the inbox-watch use case.
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), recipConv)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "DELETE: body=%s", rec.Body.String())

		// Recipient inbox no longer surfaces it (production read path).
		assert.Equal(t, 0, inboxCount(t, f.Mux, recipConv), "post-delete recipient inbox count")

		// Sender's outgoing list also no longer surfaces it — single shared
		// row, deleted once. Mirrors prune semantics the CLI already exposes.
		rows, err := db.ListAgentMessagesFromConv(senderConv, 100)
		require.NoError(t, err, "ListAgentMessagesFromConv")
		for _, m := range rows {
			assert.NotEqual(t, id, m.ID, "sender still sees deleted message id %d", id)
		}

		// Re-deleting the same id returns 404 — observable, not silent.
		r2 := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), recipConv)
		rec2 := testharness.Serve(f.Mux, r2)
		assert.Equal(t, http.StatusNotFound, rec2.Code,
			"re-delete: body=%s", rec2.Body.String())
	})
}

// Scenario: a third-party agent (NOT sender, NOT recipient) tries to
// delete someone else's message. Daemon must refuse with 403 — the
// row continues to exist for both legitimate parties.
func TestInboxDelete_ThirdPartyForbidden(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const senderConv = "snd-cccc-1111-2222-3333"
		const recipConv = "rcv-cccc-1111-2222-4444"
		const stranger = "xxx-cccc-1111-2222-5555"
		g := f.HaveGroup("alpha")
		f.HaveMember("alpha", senderConv)
		f.HaveMember("alpha", recipConv)

		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  g.ID,
			FromConv: senderConv,
			ToConv:   recipConv,
			Subject:  "private",
			Body:     "payload",
		})
		require.NoError(t, err, "InsertAgentMessage")

		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodDelete, "/v1/messages/"+strconv.FormatInt(id, 10), nil), stranger)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusForbidden, rec.Code,
			"third-party DELETE: body=%s", rec.Body.String())

		// Row must still exist for both parties.
		got, err := db.GetAgentMessage(id)
		if assert.NoError(t, err) {
			assert.NotNil(t, got, "row should still exist after third-party 403")
		}
	})
}

// inboxCount uses the same /v1/inbox endpoint the inbox-watch TUI
// polls. Asserting at this surface means we're testing the production
// read path the user actually sees.
func inboxCount(t *testing.T, mux http.Handler, convID string) int {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=100", nil), convID)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "/v1/inbox: body=%s", rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode inbox: body=%s", rec.Body.String())
	return len(out)
}
