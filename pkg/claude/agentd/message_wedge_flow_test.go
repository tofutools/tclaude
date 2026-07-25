package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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

	// The recipient takes one more turn. No prompt began during it — these rows
	// are the wedged kind — so its terminal hook releases the oldest row, with
	// no operator intervention and no new inline injection needed. One slot per
	// completed turn is the deadlock escape; releasing all ten would instead let
	// a fast sender refill the whole queue every turn.
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop", ConvID: target, Cwd: cwd,
	}, label), "ApplyHook(Stop)")

	oldest, err := db.GetAgentMessage(ids[0])
	require.NoError(t, err)
	assert.False(t, oldest.ProcessedAt.IsZero(), "the oldest wedged row is released")
	for _, id := range ids[1:] {
		message, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.True(t, message.ProcessedAt.IsZero(),
			"message #%d must still hold its slot: no prompt began this turn", id)
	}
	accepted := postMessage(t, f, sender, map[string]any{"to": target, "body": "capacity reopened"})
	require.Equal(t, http.StatusOK, accepted.Code, "body=%s", accepted.Body.String())
	var response sendRespView
	require.NoError(t, json.Unmarshal(accepted.Body.Bytes(), &response))
	assert.True(t, response.Queued)
	assert.Equal(t, regularMessageQueueLimitForTest, response.Pending,
		"exactly one slot reopened, and the new message takes it")

	// Repeating that drains the queue completely: the deadlock property is that
	// capacity always reopens, without the operator touching anything.
	for range regularMessageQueueLimitForTest {
		require.NoError(t, session.ApplyHook(session.HookCallbackInput{
			HookEventName: "Stop", ConvID: target, Cwd: cwd,
		}, label), "ApplyHook(Stop)")
	}
	for _, id := range ids {
		message, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.False(t, message.ProcessedAt.IsZero(), "message #%d drains on a later turn", id)
	}
}

// The bound must keep measuring outstanding work, not admissions per turn.
// Delivery is not gated on the pane being idle, so mail sent to a busy
// recipient queues in the harness input box; a turn that consumes one queued
// prompt must acknowledge only up to that prompt, leaving mail injected
// afterwards to hold its slot. Without the id watermark, a sender faster than
// the recipient's turn rate would be readmitted a full queue every turn.
func TestMessageBackpressureTerminalHookAcknowledgesOnlyUpToTheStartedPrompt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "wedge-rate-send-cccc-000000000001"
	const target = "wedge-rate-recv-cccc-000000000002"
	const tmux = "tclaude-wedge-rate"
	const label = "spwn-wedge-rate"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(target, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(target)
	f.HaveMember("team", sender)
	f.HaveMember("team", target)
	cwd := f.TestCwd("wedge-rate")
	f.HaveAliveSession(target, label, tmux, cwd)

	ids := make([]int64, 0, 3)
	for i := range 3 {
		rec := postMessage(t, f, sender, map[string]any{"to": target, "body": fmt.Sprintf("queued-%d", i)})
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var response sendRespView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		ids = append(ids, response.ID)
	}
	agentd.WaitForBackgroundForTest()
	agentd.FlushUndeliveredForTest(target)

	// The harness dequeues the first queued prompt; the other two are still
	// sitting in its input box when the turn ends.
	started, err := db.MarkRegularAgentMessageStarted(ids[0], target, true, time.Now())
	require.NoError(t, err)
	require.True(t, started)
	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop", ConvID: target, Cwd: cwd,
	}, label), "ApplyHook(Stop)")

	consumed, err := db.GetAgentMessage(ids[0])
	require.NoError(t, err)
	assert.False(t, consumed.ProcessedAt.IsZero(), "the prompt the recipient actually began is acknowledged")
	for _, id := range ids[1:] {
		message, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.True(t, message.ProcessedAt.IsZero(),
			"message #%d is still queued in the pane and must keep holding its slot", id)
	}
}

// A turn that ended in an API/auth/billing error consumed nothing, so
// StopFailure must not acknowledge mail — reopening the sender's capacity into
// a pane making no progress is backwards, and a rate-limited pane is still
// deliverable (isAwaitingHumanInput deliberately excludes StatusError), so mail
// keeps arriving there. The queue therefore holds through the error window and
// drains on the first successful Stop.
func TestMessageBackpressureStopFailureHoldsTheQueueUntilASuccessfulTurn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "wedge-err-send-cccc-000000000001"
	const target = "wedge-err-recv-cccc-000000000002"
	const tmux = "tclaude-wedge-err"
	const label = "spwn-wedge-err"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(target, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(target)
	f.HaveMember("team", sender)
	f.HaveMember("team", target)
	cwd := f.TestCwd("wedge-err")
	f.HaveAliveSession(target, label, tmux, cwd)

	rec := postMessage(t, f, sender, map[string]any{"to": target, "body": "sent during a rate limit"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	agentd.WaitForBackgroundForTest()
	agentd.FlushUndeliveredForTest(target)
	message, err := db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	require.False(t, message.ReadAt.IsZero(), "an erroring pane is still deliverable, so the body lands")

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "StopFailure", ConvID: target, Cwd: cwd, ErrorType: "rate_limit",
	}, label), "ApplyHook(StopFailure)")
	message, err = db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	assert.True(t, message.ProcessedAt.IsZero(),
		"a failed turn consumed nothing, so it must not reopen the sender's capacity")

	require.NoError(t, session.ApplyHook(session.HookCallbackInput{
		HookEventName: "Stop", ConvID: target, Cwd: cwd,
	}, label), "ApplyHook(Stop)")
	message, err = db.GetAgentMessage(response.ID)
	require.NoError(t, err)
	assert.False(t, message.ProcessedAt.IsZero(),
		"the first successful turn after the error window drains it, with no operator intervention")
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
