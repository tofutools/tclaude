package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// upsertConvIndexLocal mirrors the agent package's test helper: drops a
// placeholder .jsonl on disk + indexes it so ResolveSelector finds the
// conv-id by alias / prefix.
func upsertConvIndexLocal(t *testing.T, convID, customTitle string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir")
	fullPath := filepath.Join(dir, convID+".jsonl")
	require.NoError(t, os.WriteFile(fullPath, []byte(""), 0o600), "write fixture")
	mtime := time.Now().Unix()
	require.NoError(t, os.Chtimes(fullPath, time.Unix(mtime, 0), time.Unix(mtime, 0)), "chtimes")
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  dir,
		FullPath:    fullPath,
		FileMtime:   mtime,
		CustomTitle: customTitle,
		IndexedAt:   time.Now(),
	}), "UpsertConvIndex")
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
	require.NoError(t, err, "CreateAgentGroup")
	for _, c := range []string{senderConv, primaryConv, cc1Conv, cc2Conv} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: gID, ConvID: c,
		}), "AddAgentGroupMember")
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

	require.Equal(t, http.StatusOK, w.Code, "body = %s", w.Body.String())
	var resp sendResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode resp body=%s", w.Body.String())
	assert.Equal(t, "alpha", resp.ViaGroup, "ViaGroup")
	require.Len(t, resp.Recipients, 3,
		"Recipients (primary + 2 cc); got %+v", resp.Recipients)
	// Order: primary first, then cc1, cc2.
	want := []string{primaryConv, cc1Conv, cc2Conv}
	for i, w := range want {
		assert.Equal(t, w, resp.Recipients[i].ConvID, "Recipients[%d].ConvID", i)
	}

	// Verify the DB landed three rows, each with the same audience arrays.
	for _, target := range want {
		msgs, err := db.ListAgentMessagesForConv(target, 0)
		require.NoError(t, err, "ListAgentMessagesForConv(%s)", target)
		if !assert.Len(t, msgs, 1, "rows for %s", target) {
			continue
		}
		m := msgs[0]
		assert.Equal(t, "hello team", m.Body, "body for %s", target)
		assert.Equal(t, []string{primaryConv}, m.ToRecipients,
			"to_recipients for %s", target)
		assert.Equal(t, []string{cc1Conv, cc2Conv}, m.CcRecipients,
			"cc_recipients for %s", target)
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
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: gID, ConvID: c,
		}), "AddAgentGroupMember")
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
	require.Equal(t, http.StatusOK, w.Code, "body = %s", w.Body.String())

	msgs, err := db.ListAgentMessagesForConv(targetConv, 0)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, msgs, 1, "rows")
	m := msgs[0]
	assert.Equal(t, []string{targetConv}, m.ToRecipients, "ToRecipients")
	assert.Empty(t, m.CcRecipients, "CcRecipients should be empty")
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

	require.NotEqual(t, http.StatusOK, w.Code, "expected non-OK; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "CC", "error body should mention CC")
	// Crucially: NO row should have landed.
	msgs, _ := db.ListAgentMessagesForConv(primaryConv, 0)
	assert.Empty(t, msgs, "primary should have 0 rows after a rejected multi-recipient send")
}
