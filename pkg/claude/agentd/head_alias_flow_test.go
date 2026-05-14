package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, db.SetHeadAlias("po", bobV0, ""), "SetHeadAlias")
	// Reincarnation chain: bob-v0 → bob-v1 → bob-v2.
	require.NoError(t, db.RecordConvSuccession(bobV0, bobV1, "test"), "RecordConvSuccession v0→v1")
	require.NoError(t, db.RecordConvSuccession(bobV1, bobV2, "test"), "RecordConvSuccession v1→v2")

	// Resolution surface — what tryResolve does internally for any
	// caller. Bypasses the message handler so we test the resolver's
	// guarantee in isolation.
	head, err := db.ResolveHeadAlias("po")
	assert.NoError(t, err, "ResolveHeadAlias")
	assert.Equal(t, bobV2, head, "ResolveHeadAlias(po)")

	// End-to-end: Alice messages "po", and the daemon's ResolveSelector
	// should walk handle → anchor → head and land on bob-v2's inbox.
	body := map[string]any{"to": "po", "body": "head-alias smoke"}
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", body), aliceConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"POST /v1/messages body=%s", rec.Body.String())
	rows, _ := db.ListAgentMessagesForConv(bobV2, 100)
	require.Len(t, rows, 1, "Bob-v2 inbox")
	assert.Equal(t, "head-alias smoke", rows[0].Body, "body")
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
	require.Equal(t, http.StatusOK, rec.Code, "POST body=%s", rec.Body.String())
	var setResp struct {
		Handle string `json:"handle"`
		Anchor string `json:"anchor_conv_id"`
		Head   string `json:"head_conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &setResp), "decode body=%s", rec.Body.String())
	assert.Equal(t, "po", setResp.Handle, "handle (lower-cased)")

	// GET single — the daemon walks the chain at lookup time even
	// when no chain exists yet (head == anchor).
	getR := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/agent/aliases/po", nil), bobConv)
	getRec := testharness.Serve(f.Mux, getR)
	require.Equal(t, http.StatusOK, getRec.Code, "GET body=%s", getRec.Body.String())

	// LIST — open to agents (read-only metadata).
	listR := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/agent/aliases", nil), bobConv)
	listRec := testharness.Serve(f.Mux, listR)
	require.Equal(t, http.StatusOK, listRec.Code, "LIST body=%s", listRec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &rows), "decode list")
	if assert.Len(t, rows, 1, "LIST rows") {
		assert.Equal(t, "po", rows[0]["handle"], "LIST row handle")
	}

	// DELETE as human — drops the row.
	delR := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/aliases/po", nil))
	delRec := testharness.Serve(f.Mux, delR)
	require.Equal(t, http.StatusNoContent, delRec.Code, "DELETE body=%s", delRec.Body.String())
	// Re-deleting is observable as 404.
	delAgain := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/aliases/po", nil))
	delAgainRec := testharness.Serve(f.Mux, delAgain)
	assert.Equal(t, http.StatusNotFound, delAgainRec.Code, "re-DELETE")
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
	assert.Equal(t, http.StatusForbidden, rec.Code, "agent POST body=%s", rec.Body.String())

	// Pre-populate via the DB so the DELETE has something to refuse.
	require.NoError(t, db.SetHeadAlias("po", bobConv, ""), "SetHeadAlias")
	delR := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodDelete, "/v1/agent/aliases/po", nil), someConv)
	delRec := testharness.Serve(f.Mux, delR)
	assert.Equal(t, http.StatusForbidden, delRec.Code, "agent DELETE body=%s", delRec.Body.String())

	// Sanity: row still exists after the rejection.
	h, _ := db.GetHeadAlias("po")
	assert.NotNil(t, h, "row should survive a rejected agent DELETE")
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
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"handle %q: body=%s", h, rec.Body.String())
	}
}
