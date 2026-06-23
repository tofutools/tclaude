package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: Alice messages Bob using Bob's superseded conv-id (e.g. a
// raw UUID lifted from a stale terminal scrollback or an old script).
// The daemon must walk the agent_conv_succession chain forward to
// Bob-r-1 and persist the message there, recording the original
// addressee in agent_messages.original_to_conv.
//
// Pins the gap the conv-succession-chain design called out: the
// reincarnate orchestration migrates membership eagerly so title
// lookups already resolve to the live successor, but a literal
// conv-id reference still needs the chain-walk on send. Without this
// test the regression mode is "messages to a stale UUID silently
// land in an archived inbox".
func TestMessageRouting_SupersededConv_RoutesToSuccessor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const aliceConv = "alic-aaaa-bbbb-cccc-1111"
		const bobOldConv = "bobo-aaaa-bbbb-cccc-2222"
		const bobNewConv = "bobn-aaaa-bbbb-cccc-3333"

		g := f.HaveGroup("alpha")
		_ = g
		f.HaveMember("alpha", aliceConv)
		// Bob-r-1 is the live member; Bob-old has been retired and a
		// succession row points old → new. This is the state the
		// production reincarnate orchestration leaves behind.
		f.HaveMember("alpha", bobNewConv)
		require.NoError(t, db.RecordConvSuccession(bobOldConv, bobNewConv, "test"), "RecordConvSuccession")

		// Alice POSTs addressing the OLD conv-id directly (skipping the
		// title resolution path that would already point at bob-new).
		body := map[string]any{
			"to":   bobOldConv,
			"body": "Are you still there?",
		}
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/messages", body), aliceConv)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code,
			"POST /v1/messages body=%s", rec.Body.String())

		var resp struct {
			ID             int64  `json:"id"`
			ViaGroup       string `json:"via_group"`
			RedirectedFrom string `json:"redirected_from"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode response body=%s", rec.Body.String())
		assert.Equal(t, bobOldConv, resp.RedirectedFrom, "redirected_from")

		// Production read path: the message lands in Bob-r-1's inbox, NOT
		// the old conv's. Old inbox is empty.
		newRows, err := db.ListAgentMessagesForConv(bobNewConv, 100)
		require.NoError(t, err, "ListAgentMessagesForConv(new)")
		require.Len(t, newRows, 1, "Bob-new inbox")
		got := newRows[0]
		assert.Equal(t, bobOldConv, got.OriginalToConv, "OriginalToConv")
		assert.Equal(t, bobNewConv, got.ToConv, "ToConv (live successor)")

		oldRows, _ := db.ListAgentMessagesForConv(bobOldConv, 100)
		assert.Empty(t, oldRows, "Bob-old inbox should be empty (chain-walk should bypass)")
	})
}

// Scenario: the chain has multiple hops — Bob → Bob-r-1 → Bob-r-2.
// Alice messages the original Bob conv-id; ResolveLatestConv must
// walk all the way to Bob-r-2 (the head). Pins the multi-hop case
// the user flagged when reviewing the design.
func TestMessageRouting_MultiHopSuccession_FollowsToHead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const aliceConv = "alic-aaaa-bbbb-cccc-1111"
		const bobV0 = "bobv-0000-bbbb-cccc-2222"
		const bobV1 = "bobv-1111-bbbb-cccc-3333"
		const bobV2 = "bobv-2222-bbbb-cccc-4444"

		f.HaveGroup("alpha")
		f.HaveMember("alpha", aliceConv)
		f.HaveMember("alpha", bobV2) // only the head is a live member
		require.NoError(t, db.RecordConvSuccession(bobV0, bobV1, "test"), "RecordConvSuccession v0→v1")
		require.NoError(t, db.RecordConvSuccession(bobV1, bobV2, "test"), "RecordConvSuccession v1→v2")

		body := map[string]any{
			"to":   bobV0, // address the oldest ancestor
			"body": "ping the OG",
		}
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/messages", body), aliceConv)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		rows, _ := db.ListAgentMessagesForConv(bobV2, 100)
		require.Len(t, rows, 1, "Bob-v2 (head) inbox")
		assert.Equal(t, bobV0, rows[0].OriginalToConv,
			"OriginalToConv (the original ancestor, not the intermediate hop)")
		// Intermediate hop must not have collected a row.
		rowsV1, _ := db.ListAgentMessagesForConv(bobV1, 100)
		assert.Empty(t, rowsV1, "Bob-v1 (intermediate) inbox")
	})
}

// Scenario: Bob reincarnated AFTER sending Alice a message, then
// Alice replies. The reply must land in the live Bob (Bob-r-1), not
// the archived old Bob — the original message row records old Bob as
// from_conv (immutable audit trail), so the reply path needs the
// chain-walk just like the send path.
func TestReply_FromSupersededSender_RoutesToSuccessor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const aliceConv = "alic-aaaa-bbbb-cccc-1111"
		const bobOldConv = "bobo-aaaa-bbbb-cccc-2222"
		const bobNewConv = "bobn-aaaa-bbbb-cccc-3333"

		g := f.HaveGroup("alpha")
		f.HaveMember("alpha", aliceConv)
		f.HaveMember("alpha", bobNewConv)

		// Pre-existing message: Bob-old → Alice. After it was sent, Bob
		// reincarnated; the succession row records the move.
		origID, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  g.ID,
			FromConv: bobOldConv,
			ToConv:   aliceConv,
			Subject:  "earlier ping",
			Body:     "hi alice from old bob",
		})
		require.NoError(t, err, "InsertAgentMessage")
		require.NoError(t, db.RecordConvSuccession(bobOldConv, bobNewConv, "test"), "RecordConvSuccession")

		// Alice replies; daemon must rewrite the reply target old → new.
		body := map[string]any{"body": "hi! you're back"}
		path := "/v1/messages/" + itoa64(origID) + "/reply"
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, body), aliceConv)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "reply body=%s", rec.Body.String())

		var resp struct {
			ID             int64  `json:"id"`
			RedirectedFrom string `json:"redirected_from"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode reply response")
		assert.Equal(t, bobOldConv, resp.RedirectedFrom, "reply redirected_from")

		// Verify the reply landed in Bob-r-1's inbox with the original
		// (old) addressee on record.
		rows, _ := db.ListAgentMessagesForConv(bobNewConv, 100)
		require.Len(t, rows, 1, "Bob-new inbox")
		assert.Equal(t, bobOldConv, rows[0].OriginalToConv, "reply OriginalToConv")
		assert.Equal(t, origID, rows[0].ParentID,
			"reply ParentID (chain-walk must not break threading)")
	})
}

// Scenario: a succession row exists but the target is the SENDER
// itself (degenerate case where the chain-walk would route a message
// back to the caller). The send path already rejects self-sends; this
// test pins that the rejection happens AFTER the chain walk so a
// stale id pointing at the sender's own conv doesn't accidentally
// succeed.
func TestMessageRouting_RedirectsOntoSelf_Rejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const aliceConv = "alic-aaaa-bbbb-cccc-1111"
		const ghostConv = "ghos-aaaa-bbbb-cccc-9999"
		f.HaveGroup("alpha")
		f.HaveMember("alpha", aliceConv)
		require.NoError(t, db.RecordConvSuccession(ghostConv, aliceConv, "test"), "RecordConvSuccession")

		body := map[string]any{
			"to":   ghostConv, // points at alice via succession
			"body": "looping",
		}
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/messages", body), aliceConv)
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"body=%s (cannot message self)", rec.Body.String())
	})
}

// Scenario: no succession row at all — the addressed conv is its own
// canonical id. Chain-walk degrades to a no-op, OriginalToConv stays
// empty, redirected_from is omitted from the response. Pins the
// fast-path: succession infrastructure must not surface in the wire
// shape when nothing was redirected.
func TestMessageRouting_NoSuccession_NoRedirect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const aliceConv = "alic-aaaa-bbbb-cccc-1111"
		const bobConv = "bobl-aaaa-bbbb-cccc-2222"
		f.HaveGroup("alpha")
		f.HaveMember("alpha", aliceConv)
		f.HaveMember("alpha", bobConv)

		body := map[string]any{"to": bobConv, "body": "no chain here"}
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/messages", body), aliceConv)
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var resp struct {
			ID             int64  `json:"id"`
			RedirectedFrom string `json:"redirected_from"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode")
		assert.Empty(t, resp.RedirectedFrom, "redirected_from should be empty (no chain to walk)")
		rows, _ := db.ListAgentMessagesForConv(bobConv, 100)
		require.Len(t, rows, 1, "inbox")
		assert.Empty(t, rows[0].OriginalToConv, "OriginalToConv")
	})
}

// Scenario: the recipient of a message is the live successor of the
// original sender — i.e. our predecessor sent us a handoff message
// before reincarnating into us. Replying via /v1/messages/{id}/reply
// would chain-walk the reply target onto our own conv-id, which would
// land in our own inbox. Reject observably so the caller can choose a
// different action (write a fresh message, no-op, etc.).
//
// Pins a real-world scenario caught in production: a fresh agent
// reincarnated from a predecessor that just sent it the
// `reincarnation handoff` message; replying via the standard reply
// path looped the message back to the new instance.
func TestReply_ChainResolvesToSelf_Rejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const meConv = "meee-aaaa-bbbb-cccc-1111"
		const predConv = "pred-aaaa-bbbb-cccc-2222"

		g := f.HaveGroup("alpha")
		f.HaveMember("alpha", meConv)

		// Predecessor sent us a handoff message before reincarnating
		// into us. Mirrors what reincarnate's handoff path produces.
		origID, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  g.ID,
			FromConv: predConv,
			ToConv:   meConv,
			Subject:  "reincarnation handoff",
			Body:     "you are me; carry on",
		})
		require.NoError(t, err, "InsertAgentMessage")
		require.NoError(t, db.RecordConvSuccession(predConv, meConv, "reincarnate"), "RecordConvSuccession")

		body := map[string]any{"body": "thanks!"}
		path := "/v1/messages/" + itoa64(origID) + "/reply"
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, path, body), meConv)
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"body=%s (reply chain points back at sender)", rec.Body.String())
		// The reply must NOT have been written. Inbox stays at exactly
		// the predecessor's handoff row; no echo from the rejected reply.
		rows, _ := db.ListAgentMessagesForConv(meConv, 100)
		assert.Len(t, rows, 1, "inbox after rejected reply (only the original handoff)")
	})
}

// itoa64 — local helper; strconv.FormatInt is fine but inline str
// concat reads cleaner in the test.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
