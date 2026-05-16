package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

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
