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
// `substr`. Both production write paths under test (nudgeIfAlive and
// FlushUndeliveredForTest) are synchronous, so a miss here is authoritative
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

	// 1) Send while the recipient awaits human input → HELD, not delivered.
	rec := postMessage(t, f, sender, map[string]any{"to": recipient, "body": "ship it"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeSend(t, rec)
	assert.False(t, resp.Delivered, "must not be delivered while recipient awaits human input")
	assert.True(t, resp.Held, "must be reported held to the sender")
	require.NotZero(t, resp.ID, "row must still be inserted into the mailbox")

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

// TestMail_DeliversImmediatelyWhenRecipientWorking is the control: the exact
// same setup but with the recipient in a normal working state delivers inline
// at send time (delivered=true, nudge in the pane, no hold). This guards
// against the hold gate accidentally swallowing ordinary deliveries.
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
	assert.True(t, resp.Delivered, "a working recipient is delivered to inline")
	assert.False(t, resp.Held, "a working recipient is not held")

	f.AssertSentContains(recvTmux+":0.0", fmt.Sprintf("new agent message #%d", resp.ID), 2*time.Second)
	msg, err := db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	assert.False(t, msg.DeliveredAt.IsZero(), "delivered_at stamped")
}
