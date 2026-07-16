package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the Human folder's reply surface — the
// cookie-authenticated POST /api/human-messages/reply endpoint. It is the
// operator's answer to a `notify-human` ping: it resolves the raising
// agent authoritatively from the stored human_messages row, gates on that
// agent being online, and queues the reply as a sender-less operator
// message on the universal inbox.

// postDashReply POSTs /api/human-messages/reply through the dashboard mux.
func postDashReply(t *testing.T, mux http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply", body))
}

// Scenario: replying to an ONLINE agent delivers the operator's answer to
// its inbox as a sender-less message (FromConv "" = the operator), nudges
// its live pane, and marks the original notification read (the operator
// has handled it). The subject carries "Re: <original>" so the agent sees
// which ping is being answered.
func TestHumanReply_OnlineAgent_DeliversAndNudges(t *testing.T) {
	f := newFlow(t)

	const sender = "hrpl-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", sender) // enrolls the conv as an actor (agent_id)
	f.HaveAliveSession(sender, "hrpl-a", "tclaude-hrpl-a", "/tmp/work")

	msgID, err := db.InsertHumanMessage(&db.HumanMessage{
		FromConv: sender, FromTitle: "tclaude-worker", GroupName: "tclaude-dev",
		Subject: "need a decision", Body: "which option — A or B?",
	})
	require.NoError(t, err)

	mux := dashMessageMux(t)
	rec := postDashReply(t, mux, map[string]any{"id": msgID, "body": "go with option B"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		MessageID int64  `json:"message_id"`
		ConvID    string `json:"conv_id"`
		Queued    bool   `json:"queued"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.True(t, resp.Queued, "the universal inbox accepted the reply")
	assert.Equal(t, sender, resp.ConvID)

	// Real surface: the reply is in the agent's inbox as a sender-less
	// (operator) message, subject prefixed "Re: ".
	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "go with option B", rows[0].Body)
	assert.Empty(t, rows[0].FromConv, "the reply is sender-less — the mail UI renders it as the operator")
	assert.Equal(t, "Re: need a decision", rows[0].Subject)

	// The live pane is nudged over tmux.
	f.AssertSentContains("tclaude-hrpl-a:0.0", "new agent message", 2*time.Second)

	// Replying marks the original notification handled (read).
	orig, err := db.GetHumanMessage(msgID)
	require.NoError(t, err)
	require.NotNil(t, orig)
	assert.True(t, orig.IsRead(), "replying marks the answered notification read")
}

// Scenario: a notification with no subject still gets a sensible reply
// subject — the fixed "Reply from the human operator" fallback — so the
// agent sees who is speaking even when the original ping was subject-less.
func TestHumanReply_NoSubject_UsesFallback(t *testing.T) {
	f := newFlow(t)

	const sender = "hrpl-nsub-bbbb-cccc-000000000002"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", sender)
	f.HaveAliveSession(sender, "hrpl-ns", "tclaude-hrpl-ns", "/tmp/work")

	msgID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "ping, no subject"})
	require.NoError(t, err)

	mux := dashMessageMux(t)
	rec := postDashReply(t, mux, map[string]any{"id": msgID, "body": "here is my answer"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Reply from the human operator", rows[0].Subject)
}

// Scenario: replying to an OFFLINE agent is blocked with 409 — the
// operator asked that a reply not vanish into a dead session. Nothing is
// delivered and the original notification stays unread (unhandled). This
// is the server half of the gate the dialog also enforces client-side, so
// a stale snapshot can't slip a reply past.
func TestHumanReply_OfflineAgent_Rejected(t *testing.T) {
	f := newFlow(t)

	const sender = "hrpl-off0-bbbb-cccc-000000000003"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", sender)
	f.HaveAliveSession(sender, "hrpl-off", "tclaude-hrpl-off", "/tmp/work")
	f.MarkOffline("tclaude-hrpl-off") // the pane went down after the notification

	msgID, err := db.InsertHumanMessage(&db.HumanMessage{
		FromConv: sender, Subject: "still need an answer", Body: "?",
	})
	require.NoError(t, err)

	mux := dashMessageMux(t)
	rec := postDashReply(t, mux, map[string]any{"id": msgID, "body": "too late?"})
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "offline")

	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a blocked reply must not land in the inbox")

	orig, err := db.GetHumanMessage(msgID)
	require.NoError(t, err)
	require.NotNil(t, orig)
	assert.False(t, orig.IsRead(), "a blocked reply leaves the notification unhandled (unread)")
}

// Scenario: replying to an agent that is online but mid-prompt (awaiting
// human input) is ACCEPTED — it is online, just busy — but delivery is
// HELD: the reply lands in the inbox and flushes when the agent resumes,
// rather than being injected now (which the open prompt would capture as
// the human's answer). The endpoint reports queue acceptance; the async
// worker owns the readiness decision.
func TestHumanReply_BusyAgent_QueuedHeld(t *testing.T) {
	f := newFlow(t)

	const sender = "hrpl-busy-bbbb-cccc-000000000004"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", sender)
	f.HaveAliveSession(sender, "hrpl-busy", "tclaude-hrpl-busy", "/tmp/work")
	f.SetSessionStatus(sender, session.StatusAwaitingInput)

	msgID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "your question"})
	require.NoError(t, err)

	mux := dashMessageMux(t)
	rec := postDashReply(t, mux, map[string]any{"id": msgID, "body": "the answer"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Queued bool `json:"queued"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.True(t, resp.Queued, "the reply is accepted for async delivery")

	// It is still in the inbox (queued), just not yet delivered.
	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "the answer", rows[0].Body)
	assert.True(t, rows[0].DeliveredAt.IsZero(), "held: delivered_at not stamped yet")
}

func TestHumanReply_BackpressureLeavesOriginalUnhandled(t *testing.T) {
	f := newFlow(t)
	const sender = "hrpl-full-bbbb-cccc-000000000005"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", sender)
	f.HaveAliveSession(sender, "hrpl-full", "tclaude-hrpl-full", "/tmp/work")

	msgID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "your question"})
	require.NoError(t, err)
	seedRegularBacklog(t, sender, regularMessageQueueLimitForTest)

	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": msgID, "body": "the answer"})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body=%s", rec.Body.String())
	full := decodeQueueFull(t, rec.Body.Bytes())
	assert.Equal(t, sender, full.Target)

	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	assert.Len(t, rows, regularMessageQueueLimitForTest, "rejected operator reply writes no row")
	original, err := db.GetHumanMessage(msgID)
	require.NoError(t, err)
	require.NotNil(t, original)
	assert.False(t, original.IsRead(), "rejected reply leaves the notification unanswered")
}

// Scenario: replying to an id that doesn't exist is a 404 — there is no
// notification to answer.
func TestHumanReply_UnknownMessage_NotFound(t *testing.T) {
	newFlow(t)
	mux := dashMessageMux(t)
	rec := postDashReply(t, mux, map[string]any{"id": 999999, "body": "into the void"})
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the endpoint validates its inputs — a missing/zero id and an
// empty body are both 400s, caught before any resolve or delivery.
func TestHumanReply_Validation(t *testing.T) {
	f := newFlow(t)
	mux := dashMessageMux(t)

	const sender = "hrpl-vald-bbbb-cccc-000000000005"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", sender)
	f.HaveAliveSession(sender, "hrpl-v", "tclaude-hrpl-v", "/tmp/work")
	msgID, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "q"})
	require.NoError(t, err)

	// Missing id.
	rec := postDashReply(t, mux, map[string]any{"body": "no id"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// Empty body (whitespace only).
	rec = postDashReply(t, mux, map[string]any{"id": msgID, "body": "   "})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	// Nothing delivered on either rejection.
	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Scenario: a reply with no sender to route to — a human_messages row
// whose from_conv is blank (an old / human-initiated row) — is refused
// with 409, not a misleading 404 or a silent no-op.
func TestHumanReply_NoSender_Rejected(t *testing.T) {
	newFlow(t)
	mux := dashMessageMux(t)

	// A sender-less human message (from_conv "" — the human-initiated path).
	msgID, err := db.InsertHumanMessage(&db.HumanMessage{Body: "system-ish note"})
	require.NoError(t, err)

	rec := postDashReply(t, mux, map[string]any{"id": msgID, "body": "reply to whom?"})
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, strings.ToLower(rec.Body.String()), "no sender")
}
