package agentd_test

import (
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

// These tests exercise the dashboard's one-shot message surface — the
// cookie-authenticated POST /api/message endpoint. It is the browser's
// twin of `tclaude agent message`: a single immediate send, either to
// a solo agent or multicast to a whole group, funnelled through the
// same dispatchSend core POST /v1/messages uses.
//
// The human picks a From sender; the message is attributed to and
// replied to that conv. The endpoint reuses handleMulticast unchanged,
// so the group fan-out logic is not duplicated.

// dashMessageMux sets a popup base URL — so the dashboard auth's
// Origin pin is satisfiable — and returns the dashboard mux. The
// dashTestHandler then injects the cookie + Origin every /api/*
// request needs.
func dashMessageMux(t *testing.T) http.Handler {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	return agentd.BuildDashboardHandlerForTest()
}

// postDashMessage POSTs /api/message through the dashboard mux.
func postDashMessage(t *testing.T, mux http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/message", body))
}

// Scenario: a group target multicasts — every non-sender member gets
// an agent_messages row attributed to the picked From conv, and an
// alive member is nudged. The response lists one recipient per member.
func TestDashboardMessage_GroupTarget_FansOutToEveryMember(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dmsg-send-bbbb-cccc-000000000001"
	const memberA = "dmsg-aaaa-bbbb-cccc-000000000002"
	const memberB = "dmsg-bbbb-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)
	f.HaveAliveSession(memberA, "spwn-dmsg-a", "tclaude-spwn-dmsg-a", "/tmp/work")

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "dashboard broadcast",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Equal(t, "team", resp.ViaGroup)
	assert.ElementsMatch(t, []string{memberA, memberB}, recipientConvIDs(resp),
		"every non-sender member receives the dashboard multicast")

	// Real surface: each member's inbox has exactly one row, body +
	// sender intact. The sender itself is excluded from the fan-out.
	for _, m := range []string{memberA, memberB} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		require.Len(t, rows, 1, "member %s got a row", m)
		assert.Equal(t, "dashboard broadcast", rows[0].Body)
		assert.Equal(t, sender, rows[0].FromConv, "row attributed to the picked From conv")
	}
	senderRows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	assert.Empty(t, senderRows, "the From conv does not message itself")

	// The alive member is nudged over tmux.
	f.AssertSentContains("tclaude-spwn-dmsg-a:0.0", "new agent message", 2*time.Second)
}

// Scenario: a solo target reaches exactly one agent — the fan-out
// path is not taken, and a third group member gets nothing.
func TestDashboardMessage_SoloTarget_ReachesOnlyOne(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const alice = "dmsg-alic-bbbb-cccc-000000000001"
	const bob = "dmsg-bobb-bbbb-cccc-000000000002"
	const carol = "dmsg-caro-bbbb-cccc-000000000003"
	f.HaveMember("team", alice)
	f.HaveMember("team", bob)
	f.HaveMember("team", carol)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": alice, "to": bob, "body": "just for bob",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	bobRows, err := db.ListAgentMessagesForConv(bob, 100)
	require.NoError(t, err)
	require.Len(t, bobRows, 1, "bob got exactly one row")
	assert.Equal(t, "just for bob", bobRows[0].Body)
	assert.Equal(t, alice, bobRows[0].FromConv)

	carolRows, err := db.ListAgentMessagesForConv(carol, 100)
	require.NoError(t, err)
	assert.Empty(t, carolRows, "a solo send does not fan out to other members")
}

// Scenario: an empty From is a 400 before any row is written — the
// dashboard form requires the human to pick a sender.
func TestDashboardMessage_MissingFrom_BadRequest(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const member = "dmsg-mem1-bbbb-cccc-000000000001"
	f.HaveMember("team", member)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"to": "group:team", "body": "no sender",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "from is required")

	rows, err := db.ListAgentMessagesForConv(member, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected send writes no row")
}

// Scenario: a From selector matching no conversation is a 404.
func TestDashboardMessage_UnknownFrom_NotFound(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const member = "dmsg-mem2-bbbb-cccc-000000000001"
	f.HaveMember("team", member)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": "no-such-conv-anywhere", "to": "group:team", "body": "x",
	})
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())

	rows, err := db.ListAgentMessagesForConv(member, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected send writes no row")
}

// Scenario: an empty body is a 400 — dispatchSend (the shared core)
// rejects it identically to the /v1 path.
func TestDashboardMessage_EmptyBody_BadRequest(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dmsg-snd2-bbbb-cccc-000000000001"
	const member = "dmsg-mem3-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveMember("team", member)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "   ",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())

	rows, err := db.ListAgentMessagesForConv(member, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected send writes no row")
}
