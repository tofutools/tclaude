package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-737: an online, idle agent could become permanently unreachable by agent
// mail. Inline delivery stamps read_at when tmux accepts the keystrokes but
// never processed_at, and the backlog gate counts processed_at — so every
// inline message whose turn misses its terminal hook pinned a queue slot that
// no UI showed as unread. The designed self-heal (the catch-up inside
// MarkRegularAgentMessageStarted) only ran when a NEW inline message was
// injected, which the full queue rejected first: a deadlock, not a delay.
//
// Live signature confirmed on the operator's DB before the fix: ten rows with
// delivered=1, read=1, started=0, processed=0 for one recipient.
func TestMessageBackpressureInlineMailDrainsOnNextTerminalHook(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "wedge-send-bbbb-cccc-000000000001"
	const target = "wedge-recv-bbbb-cccc-000000000002"
	const tmux = "tclaude-wedge-r"
	const label = "spwn-wedge-r"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(target, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(target)
	f.HaveMember("team", sender)
	f.HaveMember("team", target)
	cwd := f.TestCwd("wedge")
	f.HaveAliveSession(target, label, tmux, cwd)

	ids := make([]int64, 0, regularMessageQueueLimitForTest)
	for i := range regularMessageQueueLimitForTest {
		rec := postMessage(t, f, sender, map[string]any{"to": target, "body": fmt.Sprintf("inline-%d", i)})
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var response sendRespView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		ids = append(ids, response.ID)
	}
	agentd.WaitForBackgroundForTest()
	agentd.FlushUndeliveredForTest(target)

	// The wedged state as production produced it: the body is in the pane and
	// the archival row reads as read, but no hook ever acknowledged the turn.
	for _, id := range ids {
		message, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		require.False(t, message.DeliveredAt.IsZero(), "message #%d was delivered inline", id)
		require.False(t, message.ReadAt.IsZero(), "inline delivery marks the archival row read on tmux acceptance")
		require.True(t, message.StartedAt.IsZero(), "the injected prompt's UserPromptSubmit correlation was missed")
		require.True(t, message.ProcessedAt.IsZero(), "no terminal hook ran for those turns")
	}

	// Nothing is unread, so neither the operator nor the recipient can see or
	// act on the backlog — yet the queue is full.
	unreadRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/inbox?unread=1&limit=100", nil), target))
	require.Equal(t, http.StatusOK, unreadRec.Code, "body=%s", unreadRec.Body.String())
	var unread []struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(unreadRec.Body.Bytes(), &unread))
	assert.Empty(t, unread, "the wedge is invisible: `inbox ls -u` shows the recipient nothing to process")
	rejected := postMessage(t, f, sender, map[string]any{"to": target, "body": "wedged"})
	require.Equal(t, http.StatusTooManyRequests, rejected.Code, "body=%s", rejected.Body.String())
	decodeQueueFull(t, rejected.Body.Bytes())

	// The recipient takes one more turn. Its terminal hook must drain every
	// read-but-unacknowledged row, with no operator intervention and no new
	// inline injection needed to trigger the catch-up.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop", ConvID: target, Cwd: cwd,
	}, label), "ApplyHook(Stop)")

	for _, id := range ids {
		message, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.False(t, message.ProcessedAt.IsZero(),
			"message #%d must be acknowledged by the recipient's terminal hook", id)
	}
	accepted := postMessage(t, f, sender, map[string]any{"to": target, "body": "capacity reopened"})
	require.Equal(t, http.StatusOK, accepted.Code, "body=%s", accepted.Body.String())
	var response sendRespView
	require.NoError(t, json.Unmarshal(accepted.Body.Bytes(), &response))
	assert.True(t, response.Queued)
	assert.Equal(t, 1, response.Pending, "the drained queue starts over at the new message")
}

// A terminal hook must not consume pointer/notification mail: that row's body
// was never put in the pane, so it still pends until `tclaude agent inbox read`
// — the contract stated in MarkReadRegularAgentMessagesProcessed's docstring.
func TestMessageBackpressureTerminalHookLeavesPointerMailPending(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "wedge-ptr-send-cccc-000000000001"
	const target = "wedge-ptr-recv-cccc-000000000002"
	const tmux = "tclaude-wedge-ptr"
	const label = "spwn-wedge-ptr"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(target, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(target)
	f.HaveMember("team", sender)
	f.HaveMember("team", target)
	cwd := f.TestCwd("wedge-ptr")
	f.HaveAliveSession(target, label, tmux, cwd)

	// A body too large to inline is announced by pointer, so the recipient must
	// fetch it explicitly. Delivery of that pointer marks nothing read.
	rec := postMessage(t, f, sender, map[string]any{"to": target, "body": pointerSizedBody()})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	agentd.WaitForBackgroundForTest()
	agentd.FlushUndeliveredForTest(target)

	message, err := db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	require.False(t, message.DeliveredAt.IsZero(), "the pointer nudge reached the pane")
	require.True(t, message.ReadAt.IsZero(), "a pointer nudge carries no body, so nothing was read")

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop", ConvID: target, Cwd: cwd,
	}, label), "ApplyHook(Stop)")
	message, err = db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	assert.True(t, message.ProcessedAt.IsZero(),
		"a terminal hook must not acknowledge mail the recipient never received the body of")
	assert.True(t, message.ReadAt.IsZero())

	require.NoError(t, db.MarkAgentMessageRead(response.ID))
	message, err = db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	assert.False(t, message.ProcessedAt.IsZero(), "explicit inbox read is what acknowledges pointer mail")
}

// pointerSizedBody exceeds config.DefaultMessageInlineMaxChars, so the nudge
// announces the message instead of carrying it.
func pointerSizedBody() string {
	return strings.Repeat("pointer body padding ", 300)
}
