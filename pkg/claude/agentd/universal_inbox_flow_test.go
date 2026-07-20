package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the "universal inbox": agent_messages is the
// transport for ALL agent→agent messages, group membership is the
// authorisation policy. Intra-group messaging is allowed by default;
// messaging off-group (a solo agent, or across a group boundary)
// requires the elevated message.direct permission and routes as a
// direct message (group_id 0).

// postMessage POSTs /v1/messages as fromConv and returns the recorder.
func postMessage(t *testing.T, f *testharness.Flow, fromConv string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/messages", body), fromConv))
}

// sendRespView is the subset of the /v1/messages response the tests assert on.
type sendRespView struct {
	ID        int64  `json:"id"`
	Queued    bool   `json:"queued"`
	Pending   int    `json:"pending"`
	Delivered bool   `json:"delivered"`
	ViaGroup  string `json:"via_group"`
}

// Scenario 1: two solo (ungrouped) agents. The sender holds
// message.direct, so the send is allowed and lands in the recipient's
// inbox as a direct message (group_id 0); an alive recipient is nudged.
func TestMessage_SoloToSolo_WithMessageDirect_Delivered(t *testing.T) {
	f := newFlow(t)

	const sender = "send-aaaa-bbbb-cccc-111111111111"
	const recip = "recp-aaaa-bbbb-cccc-222222222222"
	f.HaveConvWithTitle(sender, "sender")
	f.HaveConvWithTitle(recip, "receiver")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recip)
	f.HaveAliveSession(recip, "spwn-recp-001", "tclaude-spwn-recp-001", f.TestCwd("work"))
	require.NoError(t, db.GrantAgentPermission(sender, agentd.PermMessageDirect, "test"))

	rec := postMessage(t, f, sender, map[string]any{"to": recip, "body": "ping, solo to solo"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode body=%s", rec.Body.String())
	assert.Empty(t, resp.ViaGroup, "a direct message has no routing group")
	assert.True(t, resp.Queued, "an alive recipient: message queued for async delivery")

	rows, err := db.ListAgentMessagesForConv(recip, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "message landed in the solo recipient's inbox")
	assert.Equal(t, int64(0), rows[0].GroupID, "direct message — group_id 0")
	assert.Equal(t, "ping, solo to solo", rows[0].Body)

	agentd.WaitForBackgroundForTest() // drain the async nudge
	f.AssertSentContains("tclaude-spwn-recp-001:0.0", "new agent message", 10*time.Second)
}

// Scenario 2: solo→solo WITHOUT message.direct is refused with a 403
// that names the slug, and no message row is written.
func TestMessage_SoloToSolo_WithoutSlug_Forbidden(t *testing.T) {
	f := newFlow(t)

	const sender = "send-aaaa-bbbb-cccc-111111111111"
	const recip = "recp-aaaa-bbbb-cccc-222222222222"
	f.HaveConvWithTitle(sender, "sender")
	f.HaveConvWithTitle(recip, "receiver")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recip)

	rec := postMessage(t, f, sender, map[string]any{"to": recip, "body": "ping"})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "message.direct",
		"the 403 should name the slug to grant")

	rows, err := db.ListAgentMessagesForConv(recip, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "no message row when the send is refused")
}

// Scenario 3: two members of one group. Intra-group messaging is the
// default policy — it needs no permission slug, and the message routes
// through the shared group.
func TestMessage_IntraGroup_NoSlugNeeded(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("alpha")
	const a = "aaaa-aaaa-bbbb-cccc-111111111111"
	const b = "bbbb-aaaa-bbbb-cccc-222222222222"
	f.HaveMember("alpha", a)
	f.HaveMember("alpha", b)

	rec := postMessage(t, f, a, map[string]any{"to": b, "body": "hi teammate"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "alpha", resp.ViaGroup, "intra-group send routes via the shared group")

	rows, err := db.ListAgentMessagesForConv(b, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, g.ID, rows[0].GroupID, "row carries the routing group's id")
}

// Scenario 4: a cross-group send (A in group x, B in group y, no
// shared group, no link, no ownership) is refused without
// message.direct, and delivered as a direct message (group_id 0) once
// the slug is granted.
func TestMessage_CrossGroup_RequiresMessageDirect(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("x")
	f.HaveGroup("y")
	const a = "aaaa-aaaa-bbbb-cccc-111111111111"
	const b = "bbbb-aaaa-bbbb-cccc-222222222222"
	f.HaveMember("x", a)
	f.HaveMember("y", b)

	// Without the slug: refused.
	rec := postMessage(t, f, a, map[string]any{"to": b, "body": "cross-group ping"})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "message.direct")
	rows, err := db.ListAgentMessagesForConv(b, 100)
	require.NoError(t, err)
	require.Empty(t, rows, "refused send writes no row")

	// With the slug: delivered as a direct message.
	require.NoError(t, db.GrantAgentPermission(a, agentd.PermMessageDirect, "test"))
	rec = postMessage(t, f, a, map[string]any{"to": b, "body": "cross-group ping"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.ViaGroup, "a cross-group send is not routed through any group")

	rows, err = db.ListAgentMessagesForConv(b, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(0), rows[0].GroupID, "off-group send — group_id 0")
}

// Scenario 5: message.direct is a strict FALLBACK. A sender that holds
// the slug AND shares a group with the target still routes through the
// group — the slug never overrides group routing.
func TestMessage_MessageDirect_IsStrictFallback(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("alpha")
	const a = "aaaa-aaaa-bbbb-cccc-111111111111"
	const b = "bbbb-aaaa-bbbb-cccc-222222222222"
	f.HaveMember("alpha", a)
	f.HaveMember("alpha", b)
	// The sender holds message.direct, but a group-policy path exists.
	require.NoError(t, db.GrantAgentPermission(a, agentd.PermMessageDirect, "test"))

	rec := postMessage(t, f, a, map[string]any{"to": b, "body": "still routed via the group"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "alpha", resp.ViaGroup,
		"a shared-group path is preferred even when the sender holds message.direct")

	rows, err := db.ListAgentMessagesForConv(b, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, g.ID, rows[0].GroupID, "routed through the group, not as a direct message")
}

// Scenario 8: a reply to a direct (group_id 0) message is itself
// direct. It lands in the original sender's inbox with group_id 0 — no
// "group no longer exists" error.
func TestMessageReply_ToDirectMessage_StaysDirect(t *testing.T) {
	f := newFlow(t)

	const starter = "strt-aaaa-bbbb-cccc-111111111111"
	const responder = "resp-aaaa-bbbb-cccc-222222222222"
	f.HaveConvWithTitle(starter, "starter")
	f.HaveConvWithTitle(responder, "responder")
	f.HaveEnrolledAgent(starter)
	f.HaveEnrolledAgent(responder)
	require.NoError(t, db.GrantAgentPermission(starter, agentd.PermMessageDirect, "test"))

	// starter → responder, off-group direct message.
	rec := postMessage(t, f, starter, map[string]any{"to": responder, "body": "question?"})
	require.Equal(t, http.StatusOK, rec.Code, "send body=%s", rec.Body.String())
	var sent sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	require.NotZero(t, sent.ID)

	// responder replies.
	replyRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages/"+itoa64(sent.ID)+"/reply",
		map[string]any{"body": "answer!"}), responder))
	require.Equal(t, http.StatusOK, replyRec.Code, "reply body=%s", replyRec.Body.String())

	// The reply landed in the starter's inbox, itself a direct message.
	rows, err := db.ListAgentMessagesForConv(starter, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "reply reached the original sender's inbox")
	assert.Equal(t, int64(0), rows[0].GroupID, "reply to a direct message is itself direct")
	assert.Equal(t, "answer!", rows[0].Body)
}

// Scenario 9: a --cc send with an off-group CC. Without message.direct
// the whole send is refused (pre-validation — no rows written); with
// the slug, every row is written and the off-group CC row is direct.
func TestMessage_MultiRecipient_OffGroupCC_RequiresMessageDirect(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("alpha")
	const sender = "snd0-aaaa-bbbb-cccc-111111111111"
	const primary = "prim-aaaa-bbbb-cccc-222222222222"
	const ccTarget = "cccc-aaaa-bbbb-cccc-333333333333"
	f.HaveMember("alpha", sender)
	f.HaveMember("alpha", primary)
	f.HaveConvWithTitle(ccTarget, "outsider")
	f.HaveEnrolledAgent(ccTarget)

	// Without the slug: the off-group CC fails the whole send.
	rec := postMessage(t, f, sender, map[string]any{
		"to": primary, "body": "team note", "cc": []string{ccTarget},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "message.direct")
	pRows, err := db.ListAgentMessagesForConv(primary, 100)
	require.NoError(t, err)
	require.Empty(t, pRows, "pre-validation: no rows written when a CC fails")

	// With the slug: both the in-group primary and off-group CC land.
	require.NoError(t, db.GrantAgentPermission(sender, agentd.PermMessageDirect, "test"))
	rec = postMessage(t, f, sender, map[string]any{
		"to": primary, "body": "team note", "cc": []string{ccTarget},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	pRows, err = db.ListAgentMessagesForConv(primary, 100)
	require.NoError(t, err)
	require.Len(t, pRows, 1, "primary row written")
	assert.Equal(t, g.ID, pRows[0].GroupID, "in-group primary routes via the group")

	ccRows, err := db.ListAgentMessagesForConv(ccTarget, 100)
	require.NoError(t, err)
	require.Len(t, ccRows, 1, "off-group CC row written")
	assert.Equal(t, int64(0), ccRows[0].GroupID, "off-group CC is a direct message")
}
