package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

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
// Pins the gap the DONE/conv-succession-chain.md doc called out: the
// reincarnate orchestration migrates membership eagerly so alias
// lookups already resolve to the live successor, but a literal
// conv-id reference still needs the chain-walk on send. Without this
// test the regression mode is "messages to a stale UUID silently
// land in an archived inbox".
func TestMessageRouting_SupersededConv_RoutesToSuccessor(t *testing.T) {
	f := newFlow(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobOldConv = "bobo-aaaa-bbbb-cccc-2222"
	const bobNewConv = "bobn-aaaa-bbbb-cccc-3333"

	g := f.HaveGroup("alpha")
	_ = g
	f.HaveMember("alpha", aliceConv, "alice")
	// Bob-r-1 is the live member; Bob-old has been retired and a
	// succession row points old → new. This is the state the
	// production reincarnate orchestration leaves behind.
	f.HaveMember("alpha", bobNewConv, "bob")
	if err := db.RecordConvSuccession(bobOldConv, bobNewConv, "test"); err != nil {
		t.Fatalf("RecordConvSuccession: %v", err)
	}

	// Alice POSTs addressing the OLD conv-id directly (skipping the
	// alias resolution path that would already point at bob-new).
	body := map[string]any{
		"to":   bobOldConv,
		"body": "Are you still there?",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/messages: status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID             int64  `json:"id"`
		ViaGroup       string `json:"via_group"`
		RedirectedFrom string `json:"redirected_from"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if resp.RedirectedFrom != bobOldConv {
		t.Errorf("redirected_from = %q, want %q", resp.RedirectedFrom, bobOldConv)
	}

	// Production read path: the message lands in Bob-r-1's inbox, NOT
	// the old conv's. Old inbox is empty.
	newRows, err := db.ListAgentMessagesForConv(bobNewConv, 100)
	if err != nil {
		t.Fatalf("ListAgentMessagesForConv(new): %v", err)
	}
	if len(newRows) != 1 {
		t.Fatalf("Bob-new inbox: got %d rows, want 1", len(newRows))
	}
	got := newRows[0]
	if got.OriginalToConv != bobOldConv {
		t.Errorf("OriginalToConv = %q, want %q", got.OriginalToConv, bobOldConv)
	}
	if got.ToConv != bobNewConv {
		t.Errorf("ToConv = %q, want %q (live successor)", got.ToConv, bobNewConv)
	}

	oldRows, _ := db.ListAgentMessagesForConv(bobOldConv, 100)
	if len(oldRows) != 0 {
		t.Errorf("Bob-old inbox: got %d rows, want 0 (chain-walk should bypass)", len(oldRows))
	}
}

// Scenario: the chain has multiple hops — Bob → Bob-r-1 → Bob-r-2.
// Alice messages the original Bob conv-id; ResolveLatestConv must
// walk all the way to Bob-r-2 (the head). Pins the multi-hop case
// the user flagged when reviewing the design.
func TestMessageRouting_MultiHopSuccession_FollowsToHead(t *testing.T) {
	f := newFlow(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobV0 = "bobv-0000-bbbb-cccc-2222"
	const bobV1 = "bobv-1111-bbbb-cccc-3333"
	const bobV2 = "bobv-2222-bbbb-cccc-4444"

	f.HaveGroup("alpha")
	f.HaveMember("alpha", aliceConv, "alice")
	f.HaveMember("alpha", bobV2, "bob") // only the head is a live member
	if err := db.RecordConvSuccession(bobV0, bobV1, "test"); err != nil {
		t.Fatalf("RecordConvSuccession v0→v1: %v", err)
	}
	if err := db.RecordConvSuccession(bobV1, bobV2, "test"); err != nil {
		t.Fatalf("RecordConvSuccession v1→v2: %v", err)
	}

	body := map[string]any{
		"to":   bobV0, // address the oldest ancestor
		"body": "ping the OG",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := db.ListAgentMessagesForConv(bobV2, 100)
	if len(rows) != 1 {
		t.Fatalf("Bob-v2 (head) inbox: got %d, want 1", len(rows))
	}
	if rows[0].OriginalToConv != bobV0 {
		t.Errorf("OriginalToConv = %q, want %q (the original ancestor, not the intermediate hop)",
			rows[0].OriginalToConv, bobV0)
	}
	// Intermediate hop must not have collected a row.
	rowsV1, _ := db.ListAgentMessagesForConv(bobV1, 100)
	if len(rowsV1) != 0 {
		t.Errorf("Bob-v1 (intermediate) inbox: got %d, want 0", len(rowsV1))
	}
}

// Scenario: Bob reincarnated AFTER sending Alice a message, then
// Alice replies. The reply must land in the live Bob (Bob-r-1), not
// the archived old Bob — the original message row records old Bob as
// from_conv (immutable audit trail), so the reply path needs the
// chain-walk just like the send path.
func TestReply_FromSupersededSender_RoutesToSuccessor(t *testing.T) {
	f := newFlow(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobOldConv = "bobo-aaaa-bbbb-cccc-2222"
	const bobNewConv = "bobn-aaaa-bbbb-cccc-3333"

	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", aliceConv, "alice")
	f.HaveMember("alpha", bobNewConv, "bob")

	// Pre-existing message: Bob-old → Alice. After it was sent, Bob
	// reincarnated; the succession row records the move.
	origID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: bobOldConv,
		ToConv:   aliceConv,
		Subject:  "earlier ping",
		Body:     "hi alice from old bob",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}
	if err := db.RecordConvSuccession(bobOldConv, bobNewConv, "test"); err != nil {
		t.Fatalf("RecordConvSuccession: %v", err)
	}

	// Alice replies; daemon must rewrite the reply target old → new.
	body := map[string]any{"body": "hi! you're back"}
	path := "/v1/messages/" + itoa64(origID) + "/reply"
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("reply status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID             int64  `json:"id"`
		RedirectedFrom string `json:"redirected_from"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode reply response: %v", err)
	}
	if resp.RedirectedFrom != bobOldConv {
		t.Errorf("reply redirected_from = %q, want %q", resp.RedirectedFrom, bobOldConv)
	}

	// Verify the reply landed in Bob-r-1's inbox with the original
	// (old) addressee on record.
	rows, _ := db.ListAgentMessagesForConv(bobNewConv, 100)
	if len(rows) != 1 {
		t.Fatalf("Bob-new inbox: got %d rows, want 1", len(rows))
	}
	if rows[0].OriginalToConv != bobOldConv {
		t.Errorf("reply OriginalToConv = %q, want %q", rows[0].OriginalToConv, bobOldConv)
	}
	if rows[0].ParentID != origID {
		t.Errorf("reply ParentID = %d, want %d (chain-walk must not break threading)",
			rows[0].ParentID, origID)
	}
}

// Scenario: a succession row exists but the target is the SENDER
// itself (degenerate case where the chain-walk would route a message
// back to the caller). The send path already rejects self-sends; this
// test pins that the rejection happens AFTER the chain walk so a
// stale id pointing at the sender's own conv doesn't accidentally
// succeed.
func TestMessageRouting_RedirectsOntoSelf_Rejected(t *testing.T) {
	f := newFlow(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const ghostConv = "ghos-aaaa-bbbb-cccc-9999"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", aliceConv, "alice")
	if err := db.RecordConvSuccession(ghostConv, aliceConv, "test"); err != nil {
		t.Fatalf("RecordConvSuccession: %v", err)
	}

	body := map[string]any{
		"to":   ghostConv, // points at alice via succession
		"body": "looping",
	}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s, want 400 (cannot message self)", rec.Code, rec.Body.String())
	}
}

// Scenario: no succession row at all — the addressed conv is its own
// canonical id. Chain-walk degrades to a no-op, OriginalToConv stays
// empty, redirected_from is omitted from the response. Pins the
// fast-path: succession infrastructure must not surface in the wire
// shape when nothing was redirected.
func TestMessageRouting_NoSuccession_NoRedirect(t *testing.T) {
	f := newFlow(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobConv = "bobl-aaaa-bbbb-cccc-2222"
	f.HaveGroup("alpha")
	f.HaveMember("alpha", aliceConv, "alice")
	f.HaveMember("alpha", bobConv, "bob")

	body := map[string]any{"to": bobConv, "body": "no chain here"}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID             int64  `json:"id"`
		RedirectedFrom string `json:"redirected_from"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RedirectedFrom != "" {
		t.Errorf("redirected_from = %q, want empty (no chain to walk)", resp.RedirectedFrom)
	}
	rows, _ := db.ListAgentMessagesForConv(bobConv, 100)
	if len(rows) != 1 {
		t.Fatalf("inbox: got %d, want 1", len(rows))
	}
	if rows[0].OriginalToConv != "" {
		t.Errorf("OriginalToConv = %q, want empty", rows[0].OriginalToConv)
	}
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
