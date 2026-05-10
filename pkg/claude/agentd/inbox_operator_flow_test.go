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
	f.HaveMember("alpha", sender, "alice")
	f.HaveMember("alpha", recipient, "bob")
	// Operator is NOT a peer of either; they hold the slug instead.
	if err := db.GrantAgentPermission(operator, agentd.PermAgentInboxWatch, "test"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: sender,
		ToConv:   recipient,
		Subject:  "operator visible",
		Body:     "payload",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	// Operator-view list: header set, slug grants, expect 200 + visible.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), operator)
	r.Header.Set("X-Tclaude-Target-Conv", recipient)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator inbox: status=%d body=%s, want 200",
			rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, row := range rows {
		// JSON unmarshal of int64 → float64 in any-typed maps; tolerate.
		if rowID, _ := row["id"].(float64); int64(rowID) == id {
			found = true
		}
	}
	if !found {
		t.Errorf("operator did not see message id=%d in target inbox; rows=%+v", id, rows)
	}

	// Operator-view read: must NOT mark the row read (recipient hasn't
	// seen it yet). The endpoint defaults to mark-as-read for the
	// recipient; the operator branch must override that.
	r2 := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/messages/"+strconv.FormatInt(id, 10), nil), operator)
	r2.Header.Set("X-Tclaude-Target-Conv", recipient)
	rec2 := testharness.Serve(f.Mux, r2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("operator read: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	got, err := db.GetAgentMessage(id)
	if err != nil || got == nil {
		t.Fatalf("post-read GetAgentMessage: %v row=%v", err, got)
	}
	if !got.ReadAt.IsZero() {
		t.Errorf("operator read should NOT mark recipient's message as read; ReadAt=%v", got.ReadAt)
	}
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
	f.HaveMember("alpha", member, "bob")
	f.HaveMember("alpha", sender, "alice")
	if err := db.AddAgentGroupOwner(g.ID, owner, "test"); err != nil {
		t.Fatalf("AddAgentGroupOwner: %v", err)
	}

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: sender,
		ToConv:   member,
		Subject:  "owner visible",
		Body:     "payload",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), owner)
	r.Header.Set("X-Tclaude-Target-Conv", member)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner inbox: status=%d body=%s, want 200",
			rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	found := false
	for _, row := range rows {
		if rowID, _ := row["id"].(float64); int64(rowID) == id {
			found = true
		}
	}
	if !found {
		t.Errorf("owner did not see message id=%d in member inbox; rows=%+v", id, rows)
	}
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
	f.HaveMember("alpha", recipient, "bob")

	// Drop a message into the recipient's inbox to make the negative
	// case unambiguous (nothing else to "find").
	_, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: recipient, // self-loop is fine for the test
		ToConv:   recipient,
		Subject:  "private",
		Body:     "payload",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), stranger)
	r.Header.Set("X-Tclaude-Target-Conv", recipient)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("third-party operator: status=%d body=%s, want 403",
			rec.Code, rec.Body.String())
	}
}

// Scenario: no header at all. Self-targeted GET /v1/inbox path must
// keep working unchanged, returning the caller's own inbox.
// Regression test for the helper not breaking the non-operator path.
func TestInboxOperator_NoHeaderUsesCallerOwnInbox(t *testing.T) {
	f := newFlow(t)

	const a = "aaa-aaaa-bbbb-cccc-1111"
	const b = "bbb-aaaa-bbbb-cccc-2222"
	g := f.HaveGroup("alpha")
	f.HaveMember("alpha", a, "alice")
	f.HaveMember("alpha", b, "bob")

	// Message addressed to b. a should NOT see it without --target.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  g.ID,
		FromConv: a,
		ToConv:   b,
		Subject:  "to b",
		Body:     "payload",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/inbox?limit=50", nil), a)
	rec := testharness.Serve(f.Mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("a's own inbox: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	for _, row := range rows {
		if rowID, _ := row["id"].(float64); int64(rowID) == id {
			t.Errorf("a should NOT see message addressed to b in own inbox; rows=%+v", rows)
		}
	}
}
