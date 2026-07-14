package agentd_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent publishes an artifact with its notification. The daemon
// owns the bytes, the dashboard snapshot exposes only metadata, and the
// cookie-authenticated download returns the original file.
func TestNotifyHuman_AttachmentDownloadAndDelete(t *testing.T) {
	f := newFlow(t)
	const conv = "file-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "artifact-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	metadata, err := json.Marshal(map[string]string{
		"body": "Here is the generated report.", "subject": "artifact", "name": "report.md",
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/notify-human/attachment", strings.NewReader("# Result\n\nDone.\n"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	req = agentd.AsAgentPeer(req, conv)
	rec := testharness.Serve(f.Mux, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var delivered struct {
		ID int64 `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &delivered)

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].Attachment)
	assert.Equal(t, "report.md", msgs[0].Attachment.Filename)
	assert.Equal(t, int64(16), msgs[0].Attachment.SizeBytes)
	storedPath := msgs[0].Attachment.StoragePath

	dash := dashHandlerForTest(t)
	download := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/human-messages/"+strconv.FormatInt(delivered.ID, 10)+"/attachment", nil))
	require.Equal(t, http.StatusOK, download.Code, download.Body.String())
	assert.Equal(t, "# Result\n\nDone.\n", download.Body.String())
	assert.Contains(t, download.Header().Get("Content-Disposition"), "report.md")

	orphan := filepath.Join(filepath.Dir(storedPath), "artifact-crash-orphan")
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o600))
	staleAt := time.Now().Add(-11 * time.Minute)
	require.NoError(t, os.Chtimes(orphan, staleAt, staleAt))
	agentd.RunHumanMessageAttachmentCleanupForTest()
	_, err = os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "the reconciler removes files with no attachment metadata")
	_, err = os.Stat(storedPath)
	require.NoError(t, err, "the reconciler preserves DB-referenced bytes")

	del := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/delete", map[string]any{"id": msgs[0].ID}))
	require.Equal(t, http.StatusOK, del.Code, del.Body.String())
	_, err = os.Stat(storedPath)
	assert.True(t, os.IsNotExist(err), "deleting the message also removes its bytes")
}

func TestNotifyHuman_ZeroByteAttachment(t *testing.T) {
	f := newFlow(t)
	const conv = "zero-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "empty-file-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	rec := postNotifyAttachment(t, f.Mux, conv, "empty.txt", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].Attachment)
	assert.Zero(t, msgs[0].Attachment.SizeBytes)
}

func TestNotifyHuman_AttachmentQuotaRejectedBeforeStorage(t *testing.T) {
	f := newFlow(t)
	const conv = "quot-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "quota-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	t.Cleanup(agentd.SetHumanMessageAttachmentQuotasForTest(4, 100, 100, 1000))
	rec := postNotifyAttachment(t, f.Mux, conv, "too-big.txt", "12345")
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs)
	attachments, err := db.ListHumanMessageAttachments()
	require.NoError(t, err)
	assert.Empty(t, attachments)
}

func TestNotifyHuman_ZeroByteAttachmentsConsumeCountQuota(t *testing.T) {
	f := newFlow(t)
	const conv = "zcnt-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "zero-count-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	t.Cleanup(agentd.SetHumanMessageAttachmentQuotasForTest(1024, 4096, 1, 10))
	require.Equal(t, http.StatusOK, postNotifyAttachment(t, f.Mux, conv, "first.txt", "").Code)
	second := postNotifyAttachment(t, f.Mux, conv, "second.txt", "")
	require.Equal(t, http.StatusRequestEntityTooLarge, second.Code, second.Body.String())
}

func TestNotifyHuman_AttachmentContentTypeBounded(t *testing.T) {
	f := newFlow(t)
	const conv = "ctyp-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "content-type-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	rec := postNotifyAttachmentWithContentType(t, f.Mux, conv, "x.bin", "x", "text/plain; note="+strings.Repeat("x", 300))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestNotifyHuman_StalledAttachmentTimesOutWithoutBlockingCleanup(t *testing.T) {
	f := newFlow(t)
	const conv = "stal-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "stall-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	t.Cleanup(agentd.SetHumanMessageAttachmentUploadTimeoutForTest(40 * time.Millisecond))
	body := &blockingUploadBody{started: make(chan struct{}), closed: make(chan struct{})}
	metadata, err := json.Marshal(map[string]string{"body": "artifact ready", "name": "stalled.bin"})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/notify-human/attachment", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	req = agentd.AsAgentPeer(req, conv)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- testharness.Serve(f.Mux, req) }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("upload never began reading its request body")
	}

	cleanupDone := make(chan struct{})
	go func() {
		agentd.RunHumanMessageAttachmentCleanupForTest()
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
	case <-time.After(20 * time.Millisecond):
		t.Fatal("attachment cleanup blocked behind the stalled body read")
	}
	select {
	case rec := <-result:
		require.Equal(t, http.StatusRequestTimeout, rec.Code, rec.Body.String())
	case <-time.After(time.Second):
		t.Fatal("stalled upload did not honor its body timeout")
	}
}

type blockingUploadBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (b *blockingUploadBody) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("body closed")
}

func (b *blockingUploadBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestHumanMessageAttachmentCleanupDropsTruncatedReference(t *testing.T) {
	f := newFlow(t)
	const conv = "trun-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "truncate-maker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	rec := postNotifyAttachment(t, f.Mux, conv, "report.txt", "original")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].Attachment)
	path := msgs[0].Attachment.StoragePath
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	agentd.RunHumanMessageAttachmentCleanupForTest()
	message, err := db.GetHumanMessage(msgs[0].ID)
	require.NoError(t, err)
	require.NotNil(t, message)
	assert.Nil(t, message.Attachment, "broken bytes remove the download card but preserve the message")
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func postNotifyAttachment(t *testing.T, mux http.Handler, conv, name, data string) *httptest.ResponseRecorder {
	return postNotifyAttachmentWithContentType(t, mux, conv, name, data, "application/octet-stream")
}

func postNotifyAttachmentWithContentType(t *testing.T, mux http.Handler, conv, name, data, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	metadata, err := json.Marshal(map[string]string{"body": "artifact ready", "name": name})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/notify-human/attachment", strings.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	return testharness.Serve(mux, agentd.AsAgentPeer(req, conv))
}

// dashHandlerForTest returns the cookie-authed dashboard mux with the
// popup base URL set, so checkDashboardAuth's Origin pin is satisfied.
func dashHandlerForTest(t *testing.T) http.Handler {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	return agentd.BuildDashboardHandlerForTest()
}

// Scenario: a PO holding the human.notify slug sends a notification.
// The daemon gates on the slug, then persists the message — snapshotting
// the caller's title and group for the human-facing attribution.
func TestNotifyHuman_GrantedSenderDelivers(t *testing.T) {
	f := newFlow(t)

	const poConv = "po00-1111-2222-3333-4444"
	f.HaveConvWithTitle(poConv, "tclaude-PO")
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", poConv)
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermHumanNotify, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "CI is green; PR #142 up for review", "subject": "status"})
	r = agentd.AsAgentPeer(r, poConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1, "the message should be persisted")
	m := msgs[0]
	assert.Equal(t, "CI is green; PR #142 up for review", m.Body)
	assert.Equal(t, "status", m.Subject)
	assert.Equal(t, poConv, m.FromConv, "caller conv-id is recorded for the focus button")
	assert.Equal(t, "tclaude-PO", m.FromTitle, "caller title is snapshotted")
	assert.Equal(t, "tclaude-dev", m.GroupName, "caller group is snapshotted")
	assert.False(t, m.IsRead(), "a fresh message is unread")
}

// Scenario: an agent that owns a group may notify the human even with
// no human.notify slug — owning a group is a trusted coordinating role.
func TestNotifyHuman_GroupOwnerDelivers(t *testing.T) {
	f := newFlow(t)

	const ownerConv = "ownr-1111-2222-3333-4444"
	g := f.HaveGroup("owned-team")
	f.HaveMember("owned-team", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "owner ping, no slug"})
	r = agentd.AsAgentPeer(r, ownerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"a group owner should pass without the slug; body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1)
}

// A plain member receives human.notify from live group policy without a
// birth-time spawn profile or a copied per-agent grant.
func TestNotifyHuman_GroupPermissionDelivers(t *testing.T) {
	f := newFlow(t)

	const memberConv = "gprm-1111-2222-3333-4444"
	g := f.HaveGroup("notify-team")
	f.HaveMember("notify-team", memberConv)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermHumanNotify}, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "group policy ping"})
	r = agentd.AsAgentPeer(r, memberConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1)
}

func TestNotifyHuman_DenyOverrideBeatsGroupPermission(t *testing.T) {
	f := newFlow(t)

	const memberConv = "gpdn-1111-2222-3333-4444"
	g := f.HaveGroup("notify-team")
	f.HaveMember("notify-team", memberConv)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermHumanNotify}, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(memberConv, agentd.PermHumanNotify, db.PermEffectDeny, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "blocked group policy ping"})
	r = agentd.AsAgentPeer(r, memberConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Empty(t, msgs)
}

// Scenario: a group owner with an explicit DENY override on human.notify
// is refused — deny is always authoritative and suppresses the owner
// default, the same universal precedence every gate follows. The owner
// default only fills the "undecided" gap.
func TestNotifyHuman_DenyOverrideBeatsGroupOwner(t *testing.T) {
	f := newFlow(t)

	const ownerConv = "ownd-1111-2222-3333-4444"
	g := f.HaveGroup("owned-team")
	f.HaveMember("owned-team", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))
	require.NoError(t,
		db.SetAgentPermissionOverride(ownerConv, agentd.PermHumanNotify, db.PermEffectDeny, "test"),
		"seed deny override")

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "owner ping despite deny"})
	r = agentd.AsAgentPeer(r, ownerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a deny override must beat the group-owner default; body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Empty(t, msgs, "no message should be persisted on a denied call")
}

// Scenario: a worker that is neither a slug-holder nor a group owner is
// refused. The slug + the owner bypass are the anti-spam control;
// nothing is persisted.
func TestNotifyHuman_UngrantedWorkerForbidden(t *testing.T) {
	f := newFlow(t)

	const workerConv = "wk00-1111-2222-3333-4444"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", workerConv) // a plain member, not an owner

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "let me spam the human"})
	r = agentd.AsAgentPeer(r, workerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentd.PermHumanNotify,
		"the 403 should name the missing slug")

	msgs, _ := db.ListHumanMessages()
	assert.Empty(t, msgs, "a denied caller must not persist a message")
}

// Scenario: the human (no Claude ancestor) is implicitly allowed —
// they bypass the slug gate, same convention as every other endpoint.
func TestNotifyHuman_HumanBypasses(t *testing.T) {
	f := newFlow(t)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "human-initiated"})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1)
	assert.Empty(t, msgs[0].FromConv, "the human path has no caller conv-id")
}

// Scenario: an empty body is a client error, caught before any insert.
func TestNotifyHuman_EmptyBodyRejected(t *testing.T) {
	f := newFlow(t)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "   "})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	assert.Empty(t, msgs)
}

// Scenario: an over-long body is rejected — the size cap keeps a
// looping sender from bloating the table + every snapshot.
func TestNotifyHuman_BodyTooLongRejected(t *testing.T) {
	f := newFlow(t)

	huge := strings.Repeat("x", 32*1024)
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": huge})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "too long")

	msgs, _ := db.ListHumanMessages()
	assert.Empty(t, msgs, "an over-long body must not be persisted")
}

// Scenario: a non-POST method is refused.
func TestNotifyHuman_MethodNotAllowed(t *testing.T) {
	f := newFlow(t)
	r := testharness.JSONRequest(t, http.MethodGet, "/v1/notify-human", nil)
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: a sent message surfaces in the dashboard /api/snapshot —
// the real read surface the Messages tab renders from — with the
// unread count that drives the tab badge.
func TestNotifyHuman_AppearsInDashboardSnapshot(t *testing.T) {
	f := newFlow(t)

	const poConv = "po00-1111-2222-3333-4444"
	f.HaveConvWithTitle(poConv, "tclaude-PO")
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermHumanNotify, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "surfaces in the tab"})
	r = agentd.AsAgentPeer(r, poConv)
	require.Equal(t, http.StatusOK, testharness.Serve(f.Mux, r).Code)

	dash := dashHandlerForTest(t)
	snap := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code, "body=%s", snap.Body.String())

	var payload struct {
		Messages []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			Read bool   `json:"read"`
		} `json:"messages"`
		MessagesUnread int `json:"messages_unread"`
	}
	testharness.DecodeJSON(t, snap, &payload)
	require.Len(t, payload.Messages, 1)
	assert.Equal(t, "surfaces in the tab", payload.Messages[0].Body)
	assert.False(t, payload.Messages[0].Read)
	assert.Equal(t, 1, payload.MessagesUnread, "the unread count drives the tab badge")
}

// Scenario: the dashboard read endpoint marks one message read, then
// every message read.
func TestHumanMessages_DashboardMarkRead(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	id1, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "one"})
	require.NoError(t, err)
	_, err = db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "two"})
	require.NoError(t, err)

	// Mark one read.
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/read", map[string]any{"id": id1}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	n, _ := db.CountUnreadHumanMessages()
	assert.Equal(t, 1, n, "one of two should still be unread")

	// Mark all read.
	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/read", map[string]any{"all": true}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	n, _ = db.CountUnreadHumanMessages()
	assert.Equal(t, 0, n, "all messages should now be read")
}

// Scenario: the read endpoint's {"read": false} opt-out — the reader's
// "mark unread" toggle, the complement to the auto-mark-on-open. A message
// marked read can be flagged back to unread over the same endpoint.
func TestHumanMessages_DashboardMarkUnread(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	id, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "one"})
	require.NoError(t, err)

	// Mark it read (the omitted "read" defaults to true — what the
	// auto-mark-on-open posts).
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/read", map[string]any{"id": id}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	n, _ := db.CountUnreadHumanMessages()
	require.Equal(t, 0, n, "marked read")

	// Now flag it back to unread with {"read": false}.
	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/read", map[string]any{"id": id, "read": false}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	n, _ = db.CountUnreadHumanMessages()
	assert.Equal(t, 1, n, "the read message is unread again")

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1)
	assert.False(t, msgs[0].IsRead(), "read_at is cleared")
}

// Scenario: the dashboard clear endpoint deletes read messages and
// leaves unread ones intact.
func TestHumanMessages_DashboardClear(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	readID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "read me"})
	require.NoError(t, err)
	_, err = db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "still unread"})
	require.NoError(t, err)
	_, err = db.MarkHumanMessageRead(readID)
	require.NoError(t, err)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/clear", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1, "only the unread message should remain")
	assert.Equal(t, "still unread", msgs[0].Body)
}

// Scenario: the per-message delete endpoint removes one message by id,
// read state irrelevant — it deletes an UNREAD message and leaves the
// others (read and unread) untouched, distinct from the bulk clear.
func TestHumanMessages_DashboardDeleteOne(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	keepID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "keep me"})
	require.NoError(t, err)
	dropID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "delete me"})
	require.NoError(t, err)
	// The to-delete message is left UNREAD on purpose — proving the
	// per-message delete ignores read state, unlike the /clear sweep
	// (which only removes already-read rows).
	before, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, before, 2)
	for _, m := range before {
		require.False(t, m.IsRead(), "both messages start unread")
	}

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/delete", map[string]any{"id": dropID}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Deleted int `json:"deleted"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 1, resp.Deleted, "exactly one row removed")

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1, "only the untouched message survives")
	assert.Equal(t, keepID, msgs[0].ID)
	assert.Equal(t, "keep me", msgs[0].Body)
}

// Scenario: deleting an id that doesn't exist is a clean no-op — 200
// with deleted:0, not a 404 or 500. Mirrors the idempotent DB layer.
func TestHumanMessages_DashboardDeleteMissing(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/delete", map[string]any{"id": 999999}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Deleted int `json:"deleted"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 0, resp.Deleted, "no row matched")
}

// Scenario: the delete endpoint rejects a missing/zero id with 400 —
// the client must name a message, there is no "delete all" via this
// route (that's /clear).
func TestHumanMessages_DashboardDeleteRequiresID(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/delete", map[string]any{}))
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the delete endpoint also accepts an ids array — the Messages
// tab's multi-select delete. Read state is irrelevant (like the single
// case); only the named ids go.
func TestHumanMessages_DashboardDeleteMany(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)

	keepID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "keep me"})
	require.NoError(t, err)
	drop1, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "drop one"})
	require.NoError(t, err)
	drop2, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: "c", Body: "drop two"})
	require.NoError(t, err)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/human-messages/delete", map[string]any{"ids": []int64{drop1, drop2}}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Deleted int `json:"deleted"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 2, resp.Deleted, "both named rows removed")

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1, "only the unselected message survives")
	assert.Equal(t, keepID, msgs[0].ID)
}

// Scenario: the server stores an agent-supplied subject/body VERBATIM —
// it does NOT escape on the way in. This pins the XSS contract for the
// Messages tab: the dashboard JS `esc()` helper is the SINGLE source of
// truth for escaping, applied once at render time. The server stores
// raw on purpose — a server-side escape here would double-escape, so
// the human would see literal `&lt;script&gt;` in the tab. A future
// well-meaning "sanitize on insert" change must fail this test.
//
// (The actual XSS defense — esc() producing no live <script> sink — is
// JS and lives in the browser; this test guards the server half of the
// contract: raw in, raw out.)
func TestNotifyHuman_StoresPayloadVerbatim(t *testing.T) {
	f := newFlow(t)

	const poConv = "po00-1111-2222-3333-4444"
	f.HaveConvWithTitle(poConv, "tclaude-PO")
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermHumanNotify, "test"))

	const xssBody = `<script>alert("xss")</script> & <img src=x onerror=alert(1)> 'quoted' "dq"`
	const xssSubject = `</title><script>steal()</script> & <b>`

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": xssBody, "subject": xssSubject})
	r = agentd.AsAgentPeer(r, poConv)
	require.Equal(t, http.StatusOK, testharness.Serve(f.Mux, r).Code)

	// DB surface: stored exactly as sent — no pre-escaping, no stripping.
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, xssBody, msgs[0].Body,
		"body must be stored raw — the JS esc() escapes at render, not the server")
	assert.Equal(t, xssSubject, msgs[0].Subject, "subject must be stored raw")

	// Dashboard snapshot surface: the Messages tab renders from here.
	// JSON transport may \u-escape angle brackets on the wire, but that
	// is transport encoding — it round-trips back to the raw string on
	// decode. What the browser's JSON.parse hands the tab JS is raw, so
	// esc() genuinely is the only escaping in the pipeline.
	dash := dashHandlerForTest(t)
	snap := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code, "body=%s", snap.Body.String())
	var payload struct {
		Messages []struct {
			Body    string `json:"body"`
			Subject string `json:"subject"`
		} `json:"messages"`
	}
	testharness.DecodeJSON(t, snap, &payload)
	require.Len(t, payload.Messages, 1)
	assert.Equal(t, xssBody, payload.Messages[0].Body, "snapshot carries the body verbatim")
	assert.Equal(t, xssSubject, payload.Messages[0].Subject, "snapshot carries the subject verbatim")
}

// Scenario: a group owner passes the notify-human gate even when the
// group they own is entirely unrelated to the human-notify channel —
// and even when they own it WITHOUT being a member of it or of any
// other group, and hold no human.notify slug.
//
// notify-human is a global channel with no group to scope against, so
// the owner check in requireNotifyHumanPermission is deliberately
// UNSCOPED: owning *any* group is enough. This test pins that as
// intentional. A future refactor that "tightens" the bypass to a
// scoped check (owner of some notify-related group only) must fail
// here — the broad bypass is a choice, not an oversight.
func TestNotifyHuman_UnrelatedGroupOwnerPasses(t *testing.T) {
	f := newFlow(t)

	const ownerConv = "ownr-9999-8888-7777-6666"
	// One group, named so its irrelevance to notify-human is obvious.
	// No HaveMember: ownership alone — AddAgentGroupOwner enrolls the
	// conv as an agent — is the entire basis for the bypass.
	g := f.HaveGroup("weather-bot")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "owner of an unrelated group still reaches the human"})
	r = agentd.AsAgentPeer(r, ownerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"owning any group — related or not — passes the gate; body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	require.Len(t, msgs, 1, "the owner's message is persisted")
}

// capturedNotif records one humanMsgNotify dispatch for assertion.
type capturedNotif struct {
	senderSessionID string
	fromTitle       string
	group           string
	subject         string
	body            string
}

// Scenario: a delivered notify-human ALSO raises an OS notification — the
// desktop companion to the Messages tab. The dispatch carries the sender's
// session ID (so the banner is click-to-focus onto that agent's terminal,
// the OS twin of the dashboard Focus button), the snapshotted group, and
// the subject/body verbatim. Fired off the request goroutine via
// goBackground, so the test drains with WaitForBackgroundForTest.
func TestNotifyHuman_RaisesOSNotification(t *testing.T) {
	f := newFlow(t)

	var got []capturedNotif
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(
		func(senderSessionID, fromTitle, group, subject, body string) {
			got = append(got, capturedNotif{senderSessionID, fromTitle, group, subject, body})
		}))
	t.Cleanup(agentd.WaitForBackgroundForTest) // drain any stragglers before $HOME teardown

	const poConv = "po00-1111-2222-3333-4444"
	const poSession = "po-session-label"
	f.HaveConvWithTitle(poConv, "tclaude-PO")
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", poConv)
	// A session row so notifyHumanSenderSessionID resolves the sender's
	// session for the banner's click-to-focus target.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: poSession, ConvID: poConv, TmuxSession: "tmux-po", Cwd: "/work/proj", Status: "running",
	}))
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermHumanNotify, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "build is green", "subject": "status"})
	r = agentd.AsAgentPeer(r, poConv)
	require.Equal(t, http.StatusOK, testharness.Serve(f.Mux, r).Code)

	agentd.WaitForBackgroundForTest() // the notification fires on goBackground
	require.Len(t, got, 1, "a delivered notify-human should raise exactly one OS notification")
	n := got[0]
	assert.Equal(t, poSession, n.senderSessionID,
		"the banner click-to-focuses the sending agent's session")
	assert.Equal(t, "tclaude-dev", n.group, "the snapshotted group rides along")
	assert.Equal(t, "status", n.subject)
	assert.Equal(t, "build is green", n.body)
	assert.Equal(t, "tclaude-PO", n.fromTitle, "the snapshotted sender title rides along")
	// And it matches what was snapshotted onto the persisted row — both
	// come from notifyHumanCallerTitle(callerConv).
	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, msgs[0].FromTitle, n.fromTitle,
		"the notification's sender attribution matches the stored message")
}

// A freshly-started Codex agent can send before its conversation is indexed.
// Its spawn-time pending_name is already durable on the stable actor and must
// be snapshotted as the notification sender instead of leaving only agt_… for
// the dashboard to render.
func TestNotifyHuman_PendingNameTitlesUnindexedSender(t *testing.T) {
	f := newFlow(t)
	const conv = "notify-code-1111-2222-33334444"
	f.HaveGroup("codex-team")
	f.HaveMember("codex-team", conv)
	f.HavePendingName(conv, "codex-reviewer")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "review complete"})
	r = agentd.AsAgentPeer(r, conv)
	require.Equal(t, http.StatusOK, testharness.Serve(f.Mux, r).Code)

	msgs, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "codex-reviewer", msgs[0].FromTitle)
}

// Scenario: a refused notify-human (not a slug-holder, not a group owner)
// raises NO OS notification — the gate runs before any insert or dispatch,
// so a denied sender can't even ring the desktop.
func TestNotifyHuman_ForbiddenSenderRaisesNoNotification(t *testing.T) {
	f := newFlow(t)

	var got []capturedNotif
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(
		func(senderSessionID, fromTitle, group, subject, body string) {
			got = append(got, capturedNotif{senderSessionID, fromTitle, group, subject, body})
		}))
	t.Cleanup(agentd.WaitForBackgroundForTest)

	const workerConv = "wk00-1111-2222-3333-4444"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", workerConv) // a plain member, not an owner

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "let me ring the desktop"})
	r = agentd.AsAgentPeer(r, workerConv)
	require.Equal(t, http.StatusForbidden, testharness.Serve(f.Mux, r).Code)

	agentd.WaitForBackgroundForTest()
	assert.Empty(t, got, "a denied caller must not raise an OS notification")
}

// Scenario: a request body larger than maxNotifyHumanRequestBytes is
// rejected at the http.MaxBytesReader, before the JSON decoder buffers
// it all into daemon memory — the DoS guard the size cap implies.
// (TestNotifyHuman_BodyTooLongRejected covers the smaller, post-decode
// decoded-length cap; this covers the pre-decode wire cap.)
func TestNotifyHuman_OversizedRequestRejected(t *testing.T) {
	f := newFlow(t)

	// Far past the ~98 KiB wire cap — MaxBytesReader trips mid-decode.
	huge := strings.Repeat("x", 512*1024)
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": huge})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	msgs, _ := db.ListHumanMessages()
	assert.Empty(t, msgs, "an oversized request must not be persisted")
}
