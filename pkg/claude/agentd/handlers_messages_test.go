package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// upsertConvIndexLocal mirrors the agent package's test helper: drops a
// placeholder .jsonl on disk + indexes it so ResolveSelector finds the
// conv-id by alias / prefix.
func upsertConvIndexLocal(t *testing.T, convID, customTitle string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fullPath := filepath.Join(dir, convID+".jsonl")
	if err := os.WriteFile(fullPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mtime := time.Now().Unix()
	if err := os.Chtimes(fullPath, time.Unix(mtime, 0), time.Unix(mtime, 0)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  dir,
		FullPath:    fullPath,
		FileMtime:   mtime,
		CustomTitle: customTitle,
		IndexedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("UpsertConvIndex: %v", err)
	}
}

// TestHandleMessages_MultiRecipient_WritesOnePerRecipient drives
// /v1/messages with --cc set, then asserts: one row per To+CC, every
// row carries the same to_recipients/cc_recipients arrays, and the
// response surfaces the per-recipient delivery list.
func TestHandleMessages_MultiRecipient_WritesOnePerRecipient(t *testing.T) {
	setupTestDB(t)

	const senderConv = "aaaaaaaa-1111-2222-3333-444444444444"
	const primaryConv = "bbbbbbbb-1111-2222-3333-444444444444"
	const cc1Conv = "cccccccc-1111-2222-3333-444444444444"
	const cc2Conv = "dddddddd-1111-2222-3333-444444444444"

	upsertConvIndexLocal(t, senderConv, "sender")
	upsertConvIndexLocal(t, primaryConv, "primary")
	upsertConvIndexLocal(t, cc1Conv, "cc1")
	upsertConvIndexLocal(t, cc2Conv, "cc2")

	gID, err := db.CreateAgentGroup("alpha", "")
	if err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}
	for _, c := range []string{senderConv, primaryConv, cc1Conv, cc2Conv} {
		if err := db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: gID, ConvID: c,
		}); err != nil {
			t.Fatalf("AddAgentGroupMember: %v", err)
		}
	}

	body, _ := json.Marshal(map[string]any{
		"to":   "primary",
		"cc":   []string{"cc1", "cc2"},
		"body": "hello team",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 999, HasClaudeAncestor: true, ConvID: senderConv}))
	w := httptest.NewRecorder()
	handleMessages(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp sendResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v body=%s", err, w.Body.String())
	}
	if resp.ViaGroup != "alpha" {
		t.Errorf("ViaGroup = %q, want %q", resp.ViaGroup, "alpha")
	}
	if len(resp.Recipients) != 3 {
		t.Fatalf("Recipients len = %d, want 3 (primary + 2 cc); got %+v",
			len(resp.Recipients), resp.Recipients)
	}
	// Order: primary first, then cc1, cc2.
	want := []string{primaryConv, cc1Conv, cc2Conv}
	for i, w := range want {
		if resp.Recipients[i].ConvID != w {
			t.Errorf("Recipients[%d].ConvID = %q, want %q", i, resp.Recipients[i].ConvID, w)
		}
	}

	// Verify the DB landed three rows, each with the same audience arrays.
	for _, target := range want {
		msgs, err := db.ListAgentMessagesForConv(target, 0)
		if err != nil {
			t.Fatalf("ListAgentMessagesForConv(%s): %v", target, err)
		}
		if len(msgs) != 1 {
			t.Errorf("rows for %s: got %d, want 1", target, len(msgs))
			continue
		}
		m := msgs[0]
		if m.Body != "hello team" {
			t.Errorf("body for %s: got %q", target, m.Body)
		}
		if len(m.ToRecipients) != 1 || m.ToRecipients[0] != primaryConv {
			t.Errorf("to_recipients for %s: %v, want [%s]", target, m.ToRecipients, primaryConv)
		}
		if len(m.CcRecipients) != 2 || m.CcRecipients[0] != cc1Conv || m.CcRecipients[1] != cc2Conv {
			t.Errorf("cc_recipients for %s: %v", target, m.CcRecipients)
		}
	}
}

// TestHandleMessages_SingleRecipient_StillRecordsToRecipient covers
// the legacy/single-recipient send path: with no `cc`, the row should
// still carry to_recipients=[target] (so inbox read renders a
// consistent "To:" line) and cc_recipients empty.
func TestHandleMessages_SingleRecipient_StillRecordsToRecipient(t *testing.T) {
	setupTestDB(t)

	const senderConv = "aaaaaaaa-1111-2222-3333-444444444444"
	const targetConv = "bbbbbbbb-1111-2222-3333-444444444444"
	upsertConvIndexLocal(t, senderConv, "sender")
	upsertConvIndexLocal(t, targetConv, "target")

	gID, _ := db.CreateAgentGroup("alpha", "")
	for _, c := range []string{senderConv, targetConv} {
		if err := db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: gID, ConvID: c,
		}); err != nil {
			t.Fatalf("AddAgentGroupMember: %v", err)
		}
	}

	body, _ := json.Marshal(map[string]any{
		"to":   "target",
		"body": "ping",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 999, HasClaudeAncestor: true, ConvID: senderConv}))
	w := httptest.NewRecorder()
	handleMessages(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	msgs, err := db.ListAgentMessagesForConv(targetConv, 0)
	if err != nil {
		t.Fatalf("ListAgentMessagesForConv: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d rows, want 1", len(msgs))
	}
	m := msgs[0]
	if len(m.ToRecipients) != 1 || m.ToRecipients[0] != targetConv {
		t.Errorf("ToRecipients = %v, want [%s]", m.ToRecipients, targetConv)
	}
	if len(m.CcRecipients) != 0 {
		t.Errorf("CcRecipients should be empty, got %v", m.CcRecipients)
	}
}

// TestHandleMessages_UnknownCC_RejectsBeforeAnyInsert validates the
// pre-flight resolve: if any CC selector doesn't resolve, the entire
// send is rejected and zero rows hit the DB. (The user's pain point
// would be partial broadcasts where the primary lands but CCs go
// missing without anyone noticing.)
func TestHandleMessages_UnknownCC_RejectsBeforeAnyInsert(t *testing.T) {
	setupTestDB(t)

	const senderConv = "aaaaaaaa-1111-2222-3333-444444444444"
	const primaryConv = "bbbbbbbb-1111-2222-3333-444444444444"
	upsertConvIndexLocal(t, senderConv, "sender")
	upsertConvIndexLocal(t, primaryConv, "primary")

	gID, _ := db.CreateAgentGroup("alpha", "")
	for _, c := range []string{senderConv, primaryConv} {
		_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: c})
	}

	body, _ := json.Marshal(map[string]any{
		"to":   "primary",
		"cc":   []string{"this-conv-does-not-exist"},
		"body": "hi",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 999, HasClaudeAncestor: true, ConvID: senderConv}))
	w := httptest.NewRecorder()
	handleMessages(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("expected non-OK; got 200 body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CC") {
		t.Errorf("error body should mention CC; got %s", w.Body.String())
	}
	// Crucially: NO row should have landed.
	msgs, _ := db.ListAgentMessagesForConv(primaryConv, 0)
	if len(msgs) != 0 {
		t.Errorf("primary should have 0 rows after a rejected multi-recipient send; got %d", len(msgs))
	}
}
