package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

type sendResult struct {
	ID        int64 `json:"id"`
	Queued    bool  `json:"queued"`
	Pending   int   `json:"pending"`
	Delivered bool  `json:"delivered"`
	Held      bool  `json:"held"`
}

func decodeSend(t *testing.T, rec *httptest.ResponseRecorder) sendResult {
	t.Helper()
	var r sendResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r), "decode send resp: %s", rec.Body.String())
	return r
}

// assertNoSendKeys fails if any send-keys to `target` so far contains
// `substr`. Under the async delivery model (JOH-310) the caller must drain the
// worker (agentd.WaitForBackgroundForTest) first; once drained — or for the
// synchronous FlushUndeliveredForTest path — a miss here is authoritative
// without polling.
func assertNoSendKeys(t *testing.T, f *testharness.Flow, target, substr string) {
	t.Helper()
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target && strings.Contains(sk.Text, substr) {
			t.Fatalf("expected NO send-keys to %s containing %q, but got %+v", target, substr, f.World.Tmux.Sent())
		}
	}
}

// TestMail_HeldWhileRecipientAwaitingHumanInput is the JOH-308 round trip.
// A message addressed to an agent that is blocked on a human (awaiting_input)
// must NOT be nudged into its pane — the injected keystrokes would be captured
// by the open dialog as the human's answer, and the real notification lost.
// It is held in the mailbox (row inserted, delivered_at empty) and the sender
// is told so. A flush while still awaiting keeps holding (the gate runs before
// the claim, so a held row is never marked delivered-but-undeliverable). Once
// the agent is back to working, a flush delivers it for real.
func TestMail_HeldWhileRecipientAwaitingHumanInput(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mh01-send-bbbb-cccc-000000000001"
	const recipient = "mh01-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	const recvTmux = "tclaude-mh01-r"
	f.HaveAliveSession(recipient, "spwn-mh01-r", recvTmux, "/tmp/work")
	recvPane := recvTmux + ":0.0"

	// The worker is mid-question: CC is showing an elicitation dialog and is
	// capturing keystrokes as the human's answer.
	f.SetSessionStatus(recipient, session.StatusAwaitingInput)

	// 1) Send while the recipient awaits human input → queued; the async
	// worker then runs the hold gate and leaves it undelivered.
	rec := postMessage(t, f, sender, map[string]any{"to": recipient, "body": "ship it"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeSend(t, rec)
	require.True(t, resp.Queued, "row queued for async delivery")
	require.NotZero(t, resp.ID, "row must still be inserted into the mailbox")

	agentd.WaitForBackgroundForTest() // drain the async delivery; the hold gate fires inside it
	nudge := fmt.Sprintf("new agent message #%d", resp.ID)
	msg, err := db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.True(t, msg.DeliveredAt.IsZero(), "held message must stay undelivered")
	assertNoSendKeys(t, f, recvPane, nudge)

	// 2) A flush while STILL awaiting must keep holding.
	assert.Equal(t, 0, agentd.FlushUndeliveredForTest(recipient),
		"flush must hold while recipient still awaits human input")
	msg, err = db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	assert.True(t, msg.DeliveredAt.IsZero(), "still undelivered after a held flush")
	assertNoSendKeys(t, f, recvPane, nudge)

	// 3) The human answered; the worker is back to working. Flush delivers.
	f.SetSessionStatus(recipient, session.StatusWorking)
	assert.Equal(t, 1, agentd.FlushUndeliveredForTest(recipient),
		"flush delivers once the recipient is no longer awaiting human input")
	f.AssertSentContains(recvPane, nudge, 2*time.Second)
	msg, err = db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	assert.False(t, msg.DeliveredAt.IsZero(), "message delivered after the recipient resumed")
}

// TestMail_ReaperBackstopDeliversHeldMailAfterResume proves the time-bounded
// guarantee: a message held while the recipient was blocked on a human is
// delivered by a reaper sweep once the agent is back to working, WITHOUT the
// agent making any `tclaude agent` call of its own (the request-driven flush
// never fires here). It also exercises the awaiting_permission state
// end-to-end (the awaiting_input twin is covered above).
func TestMail_ReaperBackstopDeliversHeldMailAfterResume(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mh03-send-bbbb-cccc-000000000001"
	const recipient = "mh03-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	const recvTmux = "tclaude-mh03-r"
	f.HaveAliveSession(recipient, "spwn-mh03-r", recvTmux, "/tmp/work")
	recvPane := recvTmux + ":0.0"

	// The worker is showing a permission prompt — keystrokes injected now
	// would be captured as the y/n answer.
	f.SetSessionStatus(recipient, session.StatusAwaitingPermission)

	rec := postMessage(t, f, sender, map[string]any{"to": recipient, "body": "ship it"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeSend(t, rec)
	require.True(t, resp.Queued, "row queued for async delivery")
	nudge := fmt.Sprintf("new agent message #%d", resp.ID)
	agentd.WaitForBackgroundForTest() // async worker runs the hold gate (awaiting_permission)
	assertNoSendKeys(t, f, recvPane, nudge)

	// The human answered the prompt; the worker resumed working but makes no
	// agentd request. A reaper sweep is the only thing that can deliver now.
	f.SetSessionStatus(recipient, session.StatusWorking)
	agentd.RunReaperTickForTest(time.Now())
	agentd.WaitForBackgroundForTest() // drain the goBackground flush the tick queued

	f.AssertSentContains(recvPane, nudge, 2*time.Second)
	msg, err := db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	assert.False(t, msg.DeliveredAt.IsZero(), "reaper backstop delivers held mail after resume")
}

// TestMail_DeliversImmediatelyWhenRecipientWorking is the control: the exact
// same setup but with the recipient in a normal working state delivers (via
// the async worker — nudge in the pane, delivered_at stamped, no hold) once
// the worker drains. This guards against the hold gate accidentally swallowing
// ordinary deliveries.
func TestMail_DeliversImmediatelyWhenRecipientWorking(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mh02-send-bbbb-cccc-000000000001"
	const recipient = "mh02-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	const recvTmux = "tclaude-mh02-r"
	f.HaveAliveSession(recipient, "spwn-mh02-r", recvTmux, "/tmp/work")
	f.SetSessionStatus(recipient, session.StatusWorking)

	rec := postMessage(t, f, sender, map[string]any{"to": recipient, "body": "ship it"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeSend(t, rec)
	require.True(t, resp.Queued, "working recipient: message queued for async delivery")

	agentd.WaitForBackgroundForTest() // drain the async nudge
	f.AssertSentContains(recvTmux+":0.0", fmt.Sprintf("new agent message #%d", resp.ID), 2*time.Second)
	msg, err := db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	assert.False(t, msg.DeliveredAt.IsZero(), "delivered_at stamped by the worker")
}
