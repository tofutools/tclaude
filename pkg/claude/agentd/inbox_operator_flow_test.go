package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an operator with the agent.inbox-watch slug GETs another
// agent's inbox via the X-Tclaude-Target-Conv header. Daemon must
// resolve the target, return its messages, and (on /v1/messages/{id})
// leave the row unread so the recipient's read marker reflects only
// the recipient's own interaction. Mirrors the manager-pattern auth
// already used by lifecycle verbs (slug OR group ownership).
func TestInboxOperator_SlugLetsCallerReadAnothersInbox(t *testing.T) {
	f := newFlow(t)

	const operator = "ops-aaaa-bbbb-cccc-1111"
	const sender = "snd-aaaa-bbbb-cccc-2222"
	const recipient = "rcv-aaaa-bbbb-cccc-3333"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", sender)
	f.HaveMember("alpha", recipient)
	// Operator is NOT a peer of either; they hold the slug instead.
	require.NoError(t, db.GrantAgentPermission(operator, agentd.PermAgentInboxWatch, "test"), "grant")

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: sender,
		ToConv:   recipient,
		Subject:  "operator visible",
		Body:     "payload",
	})
	require.NoError(t, err, "InsertAgentMessage")

	// Operator-view list: header set, slug grants, expect 200 + visible.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), operator)
	r.Header.Set("X-Tclaude-Target-Conv", recipient)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"operator inbox: body=%s", rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows), "decode")
	found := false
	for _, row := range rows {
		// JSON unmarshal of int64 → float64 in any-typed maps; tolerate.
		if rowID, _ := row["id"].(float64); int64(rowID) == id {
			found = true
		}
	}
	assert.True(t, found, "operator did not see message id=%d in target inbox; rows=%+v", id, rows)

	// Operator-view read: must NOT mark the row read (recipient hasn't
	// seen it yet). The endpoint defaults to mark-as-read for the
	// recipient; the operator branch must override that.
	r2 := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/messages/"+strconv.FormatInt(id, 10), nil), operator)
	r2.Header.Set("X-Tclaude-Target-Conv", recipient)
	rec2 := testharness.Serve(f.Mux, r2)
	require.Equal(t, http.StatusOK, rec2.Code, "operator read: body=%s", rec2.Body.String())
	got, err := db.GetAgentMessage(id)
	require.NoError(t, err, "post-read GetAgentMessage")
	require.NotNil(t, got, "post-read row is nil")
	assert.True(t, got.ReadAt.IsZero(),
		"operator read should NOT mark recipient's message as read; ReadAt=%v", got.ReadAt)
}

// Scenario: a group owner (not a peer, no slug) reads a member's
// inbox. The owner-implicit-power path must grant access without an
// explicit slug, mirroring the same convention as the lifecycle
// verbs' requireCrossAgentPermission.
func TestInboxOperator_GroupOwnerImplicitAccess(t *testing.T) {
	f := newFlow(t)

	const owner = "own-aaaa-bbbb-cccc-1111"
	const member = "mem-aaaa-bbbb-cccc-2222"
	const sender = "snd-aaaa-bbbb-cccc-3333"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", member)
	f.HaveMember("alpha", sender)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "AddAgentGroupOwner")

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: sender,
		ToConv:   member,
		Subject:  "owner visible",
		Body:     "payload",
	})
	require.NoError(t, err, "InsertAgentMessage")

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), owner)
	r.Header.Set("X-Tclaude-Target-Conv", member)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "owner inbox: body=%s", rec.Body.String())
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	found := false
	for _, row := range rows {
		if rowID, _ := row["id"].(float64); int64(rowID) == id {
			found = true
		}
	}
	assert.True(t, found, "owner did not see message id=%d in member inbox; rows=%+v", id, rows)
}

// Scenario: a third-party agent (no slug, owns no group containing
// the target) tries the operator view. Must be refused with 403 — the
// header is ineffective without authorization. Pins the negative
// path so the slug + ownership remain the only allowing surfaces.
func TestInboxOperator_ThirdPartyForbidden(t *testing.T) {
	f := newFlow(t)

	const stranger = "xxx-aaaa-bbbb-cccc-1111"
	const recipient = "rcv-aaaa-bbbb-cccc-2222"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", recipient)

	// Drop a message into the recipient's inbox to make the negative
	// case unambiguous (nothing else to "find").
	_, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: recipient, // self-loop is fine for the test
		ToConv:   recipient,
		Subject:  "private",
		Body:     "payload",
	})
	require.NoError(t, err, "InsertAgentMessage")

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), stranger)
	r.Header.Set("X-Tclaude-Target-Conv", recipient)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"third-party operator: body=%s", rec.Body.String())
}

// Scenario: no header at all. Self-targeted GET /v1/inbox path must
// keep working unchanged, returning the caller's own inbox.
// Regression test for the helper not breaking the non-operator path.
func TestInboxOperator_NoHeaderUsesCallerOwnInbox(t *testing.T) {
	f := newFlow(t)

	const a = "aaa-aaaa-bbbb-cccc-1111"
	const b = "bbb-aaaa-bbbb-cccc-2222"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", a)
	f.HaveMember("alpha", b)

	// Message addressed to b. a should NOT see it without --target.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: a,
		ToConv:   b,
		Subject:  "to b",
		Body:     "payload",
	})
	require.NoError(t, err, "InsertAgentMessage")

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), a)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "a's own inbox: body=%s", rec.Body.String())
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	for _, row := range rows {
		rowID, _ := row["id"].(float64)
		assert.NotEqual(t, id, int64(rowID),
			"a should NOT see message addressed to b in own inbox; rows=%+v", rows)
	}
}
