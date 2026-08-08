package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardOperatorMessageQueuesSenderlessMail(t *testing.T) {
	f := newFlow(t)
	const target = "target-aaaa-bbbb-cccc-1111"
	f.HaveGroup("team")
	f.HaveMember("team", target)
	f.HaveConvWithTitle(target, "worker")

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{
			"to": "worker", "body": "Please check the failing test.",
		}))
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		ID      int64 `json:"id"`
		Queued  bool  `json:"queued"`
		Pending int   `json:"pending"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	agentd.WaitForBackgroundForTest()
	assert.True(t, response.Queued)
	assert.GreaterOrEqual(t, response.Pending, 1)
	m, err := db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Empty(t, m.FromConv, "operator mail must not impersonate an agent")
	assert.Empty(t, m.Subject, "an omitted subject must remain omitted")
	assert.Equal(t, target, m.ToConv)
	assert.Equal(t, "Please check the failing test.", m.Body)
	assert.False(t, m.DeliveredAt.IsZero(), "offline notification attempt leaves the nudge queue")
	assert.False(t, m.NudgeDiscardedAt.IsZero(), "offline notification attempt is recorded distinctly")
	assert.True(t, m.ReadAt.IsZero(), "durable operator mail remains unread")

	mailbox := getMailbox(t, mux, target)
	require.Len(t, mailbox, 1)
	assert.NotEmpty(t, mailbox[0].NudgeDiscardedAt)

	read := testharness.Serve(agentd.BuildHandlerForTest(), agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/messages/"+strconv.FormatInt(response.ID, 10), nil), target))
	require.Equal(t, http.StatusOK, read.Code, "body=%s", read.Body.String())
	var detail struct {
		Replyable bool   `json:"replyable"`
		ReplyTo   string `json:"reply_to"`
		ReplyCmd  string `json:"reply_cmd"`
	}
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &detail))
	assert.False(t, detail.Replyable)
	assert.Empty(t, detail.ReplyTo)
	assert.Empty(t, detail.ReplyCmd)
}

func TestDashboardOperatorMessagePreservesMultilineInline(t *testing.T) {
	f := newFlow(t)
	const target = "target-multiline-cccc-2222"
	const tmux = "operator-multiline"
	f.HaveGroup("team")
	f.HaveMember("team", target)
	f.HaveAliveSession(target, "op-multiline", tmux, f.TestCwd("work"))

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dashHandler := agentd.BuildDashboardHandlerForTest()
	const body = "Please inspect both failures.\n\tKeep this formatting in one turn."
	rec := testharness.Serve(dashHandler, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{"to": target, "body": body}))
	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())

	f.AssertSentContains(tmux+":0.0", "] "+body, 10*time.Second)
	agentd.WaitForBackgroundForTest()
	rows, err := db.ListAgentMessagesForConv(target, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].DeliveredAt.IsZero())
	assert.False(t, rows[0].ReadAt.IsZero(), "the inlined archival copy is consumed")
}

func TestDashboardOperatorMessageAnnouncesOnceToEveryLiveAgent(t *testing.T) {
	f := newFlow(t)
	const (
		multiGroup = "announce-live-multi-1111"
		otherGroup = "announce-live-other-2222"
		ungrouped  = "announce-live-loose-3333"
		offline    = "announce-offline-4444"
	)
	f.HaveGroup("red")
	f.HaveGroup("blue")
	f.HaveMember("red", multiGroup)
	f.HaveMember("blue", multiGroup)
	f.HaveMember("blue", otherGroup)
	f.HaveMember("red", offline)
	_, _, err := db.EnsureAgentForConv(ungrouped, "test")
	require.NoError(t, err)
	f.HaveAliveSession(multiGroup, "announce-multi", "tmux-announce-multi", f.TestCwd("multi"))
	f.HaveAliveSession(otherGroup, "announce-other", "tmux-announce-other", f.TestCwd("other"))
	f.HaveAliveSession(ungrouped, "announce-loose", "tmux-announce-loose", f.TestCwd("loose"))

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{
			"all_live": true, "body": "Dashboard-wide announcement.",
		}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		AllLive    bool `json:"all_live"`
		Recipients []struct {
			ConvID string `json:"conv_id"`
			Queued bool   `json:"queued"`
		} `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	assert.True(t, response.AllLive)
	require.Len(t, response.Recipients, 3)
	got := make([]string, 0, len(response.Recipients))
	for _, recipient := range response.Recipients {
		assert.True(t, recipient.Queued)
		got = append(got, recipient.ConvID)
	}
	assert.ElementsMatch(t, []string{multiGroup, otherGroup, ungrouped}, got,
		"group boundaries do not constrain the broadcast and multi-group agents are deduplicated")

	for _, convID := range []string{multiGroup, otherGroup, ungrouped} {
		rows, listErr := db.ListAgentMessagesForConv(convID, 10)
		require.NoError(t, listErr)
		require.Len(t, rows, 1, "live agent %s receives one row", convID)
		assert.Equal(t, "Dashboard-wide announcement.", rows[0].Body)
		assert.Empty(t, rows[0].FromConv)
		assert.True(t, db.IsOperatorAgentMessage(rows[0].ID))
	}
	offlineRows, err := db.ListAgentMessagesForConv(offline, 10)
	require.NoError(t, err)
	assert.Empty(t, offlineRows, "offline agents are not queued")
}

func TestDashboardOperatorMessageRequiresDashboardAuth(t *testing.T) {
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{"to": "nobody", "body": "hello"}))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
