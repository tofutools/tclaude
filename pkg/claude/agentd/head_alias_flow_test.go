package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: a human sets a head alias `po` → Bob, Bob then reincarnates
// (twice). Messaging `po` must always land on the live head, regardless
// of how many hops the chain has — that's the whole point of the
// global naming layer.
//
// Pins the resolver integration: tryResolve checks the head-alias
// table first; ResolveHeadAlias walks the succession chain forward via
// ResolveLatestConv. The handle row never has to be re-pointed on
// reincarnate because the chain is the source of truth.
func TestHeadAlias_SurvivesReincarnationChain(t *testing.T) {
	f := newFlow(t)

	const aliceConv = "alic-aaaa-bbbb-cccc-1111"
	const bobV0 = "bobv-0000-bbbb-cccc-2222"
	const bobV1 = "bobv-1111-bbbb-cccc-3333"
	const bobV2 = "bobv-2222-bbbb-cccc-4444"

	g := f.HaveGroup("alpha")
	_ = g
	f.HaveMember("alpha", aliceConv, "alice")
	f.HaveMember("alpha", bobV2, "bob") // only the head is a live group member

	// Human sets the global handle pointing at the original Bob.
	if err := db.SetHeadAlias("po", bobV0, ""); err != nil {
		t.Fatalf("SetHeadAlias: %v", err)
	}
	// Reincarnation chain: bob-v0 → bob-v1 → bob-v2.
	if err := db.RecordConvSuccession(bobV0, bobV1, "test"); err != nil {
		t.Fatalf("RecordConvSuccession v0→v1: %v", err)
	}
	if err := db.RecordConvSuccession(bobV1, bobV2, "test"); err != nil {
		t.Fatalf("RecordConvSuccession v1→v2: %v", err)
	}

	// Resolution surface — what tryResolve does internally for any
	// caller. Bypasses the message handler so we test the resolver's
	// guarantee in isolation.
	head, err := db.ResolveHeadAlias("po")
	if err != nil || head != bobV2 {
		t.Errorf("ResolveHeadAlias(po) = (%q, %v), want (%q, nil)", head, err, bobV2)
	}

	// End-to-end: Alice messages "po", and the daemon's ResolveSelector
	// should walk handle → anchor → head and land on bob-v2's inbox.
	body := map[string]any{"to": "po", "body": "head-alias smoke"}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/messages: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := db.ListAgentMessagesForConv(bobV2, 100)
	if len(rows) != 1 {
		t.Fatalf("Bob-v2 inbox: got %d, want 1", len(rows))
	}
	if rows[0].Body != "head-alias smoke" {
		t.Errorf("body = %q, want %q", rows[0].Body, "head-alias smoke")
	}
}

// Scenario: the daemon's mutation endpoints. POST sets, GET reads,
// DELETE drops. Pins the human-only gate (agent peers must be
// rejected) and the validation rules (UUID-shaped handles refused
// up-front so they never shadow conv-id selectors).
func TestHeadAlias_DaemonEndpointsHappyPath(t *testing.T) {
	f := newFlow(t)

	const bobConv = "bobl-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(bobConv, "bob")
	f.HaveAliveSession(bobConv, "spwn-bob", "tmux-bob", "/tmp/bob")

	// POST as human (no claude ancestor) — gates pass.
	body := map[string]any{"handle": "PO", "conv": bobConv} // upper-cased; daemon lowers
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/agent/aliases", body))
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var setResp struct {
		Handle string `json:"handle"`
		Anchor string `json:"anchor_conv_id"`
		Head   string `json:"head_conv_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setResp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if setResp.Handle != "po" {
		t.Errorf("handle = %q, want %q (lower-cased)", setResp.Handle, "po")
	}

	// GET single — the daemon walks the chain at lookup time even
	// when no chain exists yet (head == anchor).
	getR := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/agent/aliases/po", nil), bobConv)
	getRec := testharness.Serve(f.Mux, getR)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET: status=%d body=%s", getRec.Code, getRec.Body.String())
	}

	// LIST — open to agents (read-only metadata).
	listR := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/agent/aliases", nil), bobConv)
	listRec := testharness.Serve(f.Mux, listR)
	if listRec.Code != http.StatusOK {
		t.Fatalf("LIST: status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 1 || rows[0]["handle"] != "po" {
		t.Errorf("LIST rows = %+v, want one with handle=po", rows)
	}

	// DELETE as human — drops the row.
	delR := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/aliases/po", nil))
	delRec := testharness.Serve(f.Mux, delR)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d body=%s", delRec.Code, delRec.Body.String())
	}
	// Re-deleting is observable as 404.
	delAgain := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/aliases/po", nil))
	delAgainRec := testharness.Serve(f.Mux, delAgain)
	if delAgainRec.Code != http.StatusNotFound {
		t.Errorf("re-DELETE: status=%d, want 404", delAgainRec.Code)
	}
}

// Scenario: an agent (claude ancestor) tries to mutate the head-alias
// table. Without a slug shipped yet, the daemon must reject with 403.
// Pins the human-only contract — agents who somehow grab the daemon
// socket can't squat on global handles to redirect peer messages
// to themselves.
func TestHeadAlias_AgentMutationsRejected(t *testing.T) {
	f := newFlow(t)
	const someConv = "some-aaaa-bbbb-cccc-1111"
	const bobConv = "bobl-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(bobConv, "bob")

	// POST as agent — must 403.
	body := map[string]any{"handle": "po", "conv": bobConv}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/agent/aliases", body), someConv)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("agent POST: status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}

	// Pre-populate via the DB so the DELETE has something to refuse.
	if err := db.SetHeadAlias("po", bobConv, ""); err != nil {
		t.Fatalf("SetHeadAlias: %v", err)
	}
	delR := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/aliases/po", nil), someConv)
	delRec := testharness.Serve(f.Mux, delR)
	if delRec.Code != http.StatusForbidden {
		t.Errorf("agent DELETE: status=%d body=%s, want 403", delRec.Code, delRec.Body.String())
	}

	// Sanity: row still exists after the rejection.
	if h, _ := db.GetHeadAlias("po"); h == nil {
		t.Errorf("row should survive a rejected agent DELETE")
	}
}

// Scenario: validation rejects unsafe handles before they hit the DB.
// UUID-shaped strings must be refused — they'd shadow conv-id
// selectors. `group:foo` must be refused — it's the multicast prefix.
// `.` and `-` are reserved self-selectors. Pins the validator gate
// at the API surface.
func TestHeadAlias_ValidationRejectsUnsafeHandles(t *testing.T) {
	f := newFlow(t)
	const bobConv = "bobl-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(bobConv, "bob")

	cases := []string{
		"00000000-1111-2222-3333-444444444444", // looks like a conv-id
		"group:alpha",                          // multicast prefix
		".",                                    // self-selector
		"-",                                    // self-selector
		"with spaces",                          // whitespace
		"slash/in-it",                          // path separator
		"",                                     // empty
	}
	for _, h := range cases {
		body := map[string]any{"handle": h, "conv": bobConv}
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/agent/aliases", body))
		rec := testharness.Serve(f.Mux, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("handle %q: status=%d body=%s, want 400", h, rec.Code, rec.Body.String())
		}
	}
}
