package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const regularMessageQueueLimitForTest = 10

type queueFullResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Target    string `json:"target"`
	Pending   int    `json:"pending"`
	Limit     int    `json:"limit"`
	Retryable bool   `json:"retryable"`
}

type backpressureRecipientView struct {
	MessageID int64
	Queued    bool
	QueueFull bool
	Pending   int
	Limit     int
	Error     string
}

func seedRegularBacklog(t *testing.T, target string, count int) []int64 {
	t.Helper()
	ids := make([]int64, 0, count)
	for i := range count {
		id, _, err := db.InsertAgentMessageBounded(&db.AgentMessage{ToConv: target, Body: fmt.Sprintf("queued-%d", i)}, regularMessageQueueLimitForTest)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

func decodeQueueFull(t *testing.T, recBody []byte) queueFullResponse {
	t.Helper()
	var out queueFullResponse
	require.NoError(t, json.Unmarshal(recBody, &out))
	assert.Equal(t, "queue_full", out.Code)
	assert.Equal(t, regularMessageQueueLimitForTest, out.Pending)
	assert.Equal(t, regularMessageQueueLimitForTest, out.Limit)
	assert.True(t, out.Retryable)
	assert.Contains(t, out.Error, "no message was queued")
	assert.Contains(t, out.Error, "unprocessed regular messages")
	assert.Contains(t, out.Error, "process or read pending messages")
	assert.Contains(t, out.Error, "then retry")
	return out
}

func TestMessageBackpressureDirectRejectsWithoutDiscardAndReopens(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "bp-direct-sender"
	const target = "bp-direct-target"
	f.HaveMember("team", sender)
	f.HaveMember("team", target)
	ids := seedRegularBacklog(t, target, regularMessageQueueLimitForTest)

	rejected := postMessage(t, f, sender, map[string]any{"to": target, "body": "over capacity"})
	require.Equal(t, http.StatusTooManyRequests, rejected.Code, "body=%s", rejected.Body.String())
	full := decodeQueueFull(t, rejected.Body.Bytes())
	assert.Equal(t, target, full.Target)
	rows, err := db.ListAgentMessagesForConv(target, 100)
	require.NoError(t, err)
	assert.Len(t, rows, regularMessageQueueLimitForTest, "rejected send writes no row and discards nothing")

	require.NoError(t, db.MarkAgentMessageDelivered(ids[0]))
	stillFull := postMessage(t, f, sender, map[string]any{"to": target, "body": "delivery is not processing"})
	require.Equal(t, http.StatusTooManyRequests, stillFull.Code, "body=%s", stillFull.Body.String())
	require.NoError(t, db.MarkAgentMessageRead(ids[0]))
	accepted := postMessage(t, f, sender, map[string]any{"to": target, "body": "capacity reopened"})
	require.Equal(t, http.StatusOK, accepted.Code, "body=%s", accepted.Body.String())
	var response sendRespView
	require.NoError(t, json.Unmarshal(accepted.Body.Bytes(), &response))
	assert.True(t, response.Queued)
	assert.Equal(t, regularMessageQueueLimitForTest, response.Pending)
}

func TestMessageBackpressureReplyAndDashboardShareActionableError(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const target = "bp-shared-target"
	const replier = "bp-shared-replier"
	f.HaveMember("team", target)
	f.HaveMember("team", replier)
	originalID, err := db.InsertAgentMessage(&db.AgentMessage{FromConv: target, ToConv: replier, Body: "please reply"})
	require.NoError(t, err)
	seedRegularBacklog(t, target, regularMessageQueueLimitForTest)

	replyReq := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/messages/"+strconv.FormatInt(originalID, 10)+"/reply", map[string]any{"body": "reply"}), replier)
	replyRec := testharness.Serve(f.Mux, replyReq)
	require.Equal(t, http.StatusTooManyRequests, replyRec.Code, "body=%s", replyRec.Body.String())
	replyFull := decodeQueueFull(t, replyRec.Body.Bytes())

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dashboardRec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodPost,
		"/api/operator-message", map[string]any{"to": target, "body": "operator send"}))
	require.Equal(t, http.StatusTooManyRequests, dashboardRec.Code, "body=%s", dashboardRec.Body.String())
	dashboardFull := decodeQueueFull(t, dashboardRec.Body.Bytes())
	assert.Equal(t, replyFull.Error, dashboardFull.Error, "one-shot surfaces share queue-full wording")

	rows, err := db.ListAgentMessagesForConv(target, 100)
	require.NoError(t, err)
	assert.Len(t, rows, regularMessageQueueLimitForTest,
		"neither rejected reply nor dashboard send writes a target row")
}

func TestMessageBackpressureGroupReportsPerRecipientAndPreservesSuccess(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "bp-group-sender"
	const fullTarget = "bp-group-full"
	const freeTarget = "bp-group-free"
	f.HaveMember("team", sender)
	f.HaveMember("team", fullTarget)
	f.HaveMember("team", freeTarget)
	seedRegularBacklog(t, fullTarget, regularMessageQueueLimitForTest)

	rec := postMessage(t, f, sender, map[string]any{"to": "group:team", "body": "broadcast"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		Recipients []struct {
			ConvID    string `json:"conv_id"`
			MessageID int64  `json:"message_id"`
			Queued    bool   `json:"queued"`
			QueueFull bool   `json:"queue_full"`
			Pending   int    `json:"pending"`
			Limit     int    `json:"limit"`
			Error     string `json:"error"`
		} `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Len(t, response.Recipients, 2)
	byTarget := make(map[string]backpressureRecipientView, 2)
	for _, recipient := range response.Recipients {
		byTarget[recipient.ConvID] = backpressureRecipientView{
			MessageID: recipient.MessageID, Queued: recipient.Queued, QueueFull: recipient.QueueFull,
			Pending: recipient.Pending, Limit: recipient.Limit, Error: recipient.Error,
		}
	}
	full := byTarget[fullTarget]
	assert.Zero(t, full.MessageID)
	assert.False(t, full.Queued)
	assert.True(t, full.QueueFull)
	assert.Equal(t, regularMessageQueueLimitForTest, full.Pending)
	assert.Equal(t, regularMessageQueueLimitForTest, full.Limit)
	assert.Contains(t, full.Error, "then retry")
	free := byTarget[freeTarget]
	assert.NotZero(t, free.MessageID)
	assert.True(t, free.Queued)
	assert.False(t, free.QueueFull)

	fullRows, err := db.ListAgentMessagesForConv(fullTarget, 100)
	require.NoError(t, err)
	assert.Len(t, fullRows, regularMessageQueueLimitForTest, "full recipient gets no partial row")
	freeRows, err := db.ListAgentMessagesForConv(freeTarget, 100)
	require.NoError(t, err)
	require.Len(t, freeRows, 1, "available recipient still receives the broadcast")
	assert.Equal(t, "broadcast", freeRows[0].Body)
}

func TestMessageBackpressureCCContinuesWhenPrimaryIsFull(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "bp-cc-sender"
	const fullPrimary = "bp-cc-full"
	const freeCC = "bp-cc-free"
	f.HaveMember("team", sender)
	f.HaveMember("team", fullPrimary)
	f.HaveMember("team", freeCC)
	seedRegularBacklog(t, fullPrimary, regularMessageQueueLimitForTest)

	rec := postMessage(t, f, sender, map[string]any{
		"to": fullPrimary, "cc": []string{freeCC}, "body": "copy this",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		Recipients []struct {
			ConvID    string `json:"conv_id"`
			MessageID int64  `json:"message_id"`
			Queued    bool   `json:"queued"`
			QueueFull bool   `json:"queue_full"`
		} `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Len(t, response.Recipients, 2)
	assert.Equal(t, fullPrimary, response.Recipients[0].ConvID)
	assert.True(t, response.Recipients[0].QueueFull)
	assert.False(t, response.Recipients[0].Queued)
	assert.Equal(t, freeCC, response.Recipients[1].ConvID)
	assert.True(t, response.Recipients[1].Queued)
	assert.NotZero(t, response.Recipients[1].MessageID)

	rows, err := db.ListAgentMessagesForConv(freeCC, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "copy this", rows[0].Body)
}

func TestMessageBackpressureOfflineSuppressesNudgesWithoutReplayOrCapacityLoss(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "bp-offline-sender"
	const target = "bp-offline-target"
	const tmux = "tclaude-bp-offline"
	f.HaveMember("team", sender)
	f.HaveMember("team", target)

	ids := make([]int64, 0, regularMessageQueueLimitForTest)
	for i := range regularMessageQueueLimitForTest {
		rec := postMessage(t, f, sender, map[string]any{"to": target, "body": fmt.Sprintf("offline-%d", i)})
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var response sendRespView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		ids = append(ids, response.ID)
	}
	agentd.WaitForBackgroundForTest()

	for _, id := range ids {
		message, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.True(t, message.RegularSend)
		assert.False(t, message.DeliveredAt.IsZero(), "offline nudge leaves the first-delivery queue")
		assert.False(t, message.NudgeDiscardedAt.IsZero(), "offline notification attempt is explicitly discarded")
		assert.True(t, message.ReadAt.IsZero(), "durable inbox row remains unread")
		assert.True(t, message.ProcessedAt.IsZero(), "suppression does not free backlog capacity")
	}
	outboxRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/inbox?outbox=1&limit=20", nil), sender))
	require.Equal(t, http.StatusOK, outboxRec.Code, "body=%s", outboxRec.Body.String())
	var outbox []struct {
		Delivered        bool   `json:"delivered"`
		NudgeDiscardedAt string `json:"nudge_discarded_at"`
	}
	require.NoError(t, json.Unmarshal(outboxRec.Body.Bytes(), &outbox))
	require.Len(t, outbox, regularMessageQueueLimitForTest)
	for _, item := range outbox {
		assert.False(t, item.Delivered, "suppressed notification is not presented as delivered")
		assert.NotEmpty(t, item.NudgeDiscardedAt)
	}

	overflow := postMessage(t, f, sender, map[string]any{"to": target, "body": "eleventh"})
	require.Equal(t, http.StatusTooManyRequests, overflow.Code, "body=%s", overflow.Body.String())
	decodeQueueFull(t, overflow.Body.Bytes())

	f.HaveAliveSession(target, "spwn-bp-offline", tmux, f.TestCwd("work"))
	before := len(f.World.Tmux.Sent())
	assert.Zero(t, agentd.FlushUndeliveredForTest(target), "suppressed regular nudges never re-enter first delivery")
	for _, sent := range f.World.Tmux.Sent()[before:] {
		assert.False(t, strings.Contains(sent.Text, "new agent message #"),
			"resume must not burst per-message nudges: %q", sent.Text)
	}

	rows, err := db.ListAgentMessagesForConv(target, 100)
	require.NoError(t, err)
	assert.Len(t, rows, regularMessageQueueLimitForTest)
	for _, message := range rows {
		assert.True(t, message.ReadAt.IsZero())
		assert.True(t, message.ProcessedAt.IsZero())
	}
}

func TestInboxReadRecoversAlreadyReadRegularMessageAfterMissedHook(t *testing.T) {
	f := newFlow(t)
	const target = "bp-missed-hook-read-target"
	f.HaveEnrolledAgent(target)

	id, _, err := db.InsertAgentMessageBounded(&db.AgentMessage{ToConv: target, Body: "inline body"}, 1)
	require.NoError(t, err)
	claim, claimed, err := db.ClaimAgentMessageNudge(id, time.Now())
	require.NoError(t, err)
	require.True(t, claimed)
	completed, err := db.CompleteAgentMessageNudgeState(id, claim, time.Now(), true)
	require.NoError(t, err)
	require.True(t, completed)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d", id), nil), target))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	message, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.False(t, message.ProcessedAt.IsZero(), "explicit read acknowledges an already-read regular row")
	_, _, err = db.InsertAgentMessageBounded(&db.AgentMessage{ToConv: target, Body: "capacity reopened"}, 1)
	require.NoError(t, err)
}
