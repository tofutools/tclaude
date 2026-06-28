package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests pin the async, per-AGENT nudge delivery queue (JOH-310): the
// sender returns immediately with a queue depth and never blocks on tmux; the
// per-agent worker drains the durable queue, keyed on the stable agent_id so a
// message survives the recipient reincarnating; and an explicit `gen` pins a
// message to a specific past generation instead of following the head.

// TestNudgeQueue_SenderReturnsImmediately_WithDepth: sending to an OFFLINE
// recipient returns queued=true with a growing pending depth and delivers
// nothing (no claim, no nudge) until the recipient comes online — at which
// point a drain delivers the whole backlog, oldest first.
func TestNudgeQueue_SenderReturnsImmediately_WithDepth(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "nq01-send-bbbb-cccc-000000000001"
	const recipient = "nq01-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recipient)
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	// recipient is intentionally OFFLINE (no alive session).

	r1 := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "one"})
	assert.True(t, r1.Queued, "queued even though the recipient is offline")
	assert.Equal(t, 1, r1.Pending, "first message: queue depth 1")

	r2 := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "two"})
	assert.True(t, r2.Queued)
	assert.Equal(t, 2, r2.Pending, "second message: queue depth 2")

	// Draining while offline must NOT claim/lose the backlog (the gate runs
	// before the claim).
	agentd.WaitForBackgroundForTest()
	for _, id := range []int64{r1.ID, r2.ID} {
		m, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.True(t, m.DeliveredAt.IsZero(), "offline recipient: message #%d stays undelivered", id)
	}

	// Recipient comes online; a drain delivers the whole backlog, oldest first.
	const tmux = "tclaude-nq01-r"
	f.HaveAliveSession(recipient, "spwn-nq01-r", tmux, "/tmp/work")
	assert.Equal(t, 2, agentd.FlushUndeliveredForTest(recipient), "both delivered once online")
	f.AssertSentContains(tmux+":0.0", fmt.Sprintf("new agent message #%d", r1.ID), 2*time.Second)
	f.AssertSentContains(tmux+":0.0", fmt.Sprintf("new agent message #%d", r2.ID), 2*time.Second)
}

// TestNudgeQueue_SurvivesReincarnation is the headline correctness win: a
// message queued to an agent while it is offline is delivered to the agent's
// CURRENT head generation even after the conv-id rotated (reincarnate / /clear)
// between send and delivery. Keying on agent_id — not the to_conv recorded at
// send time — is what makes this work; the pre-async conv-keyed flush would
// have stranded it on the dead generation.
func TestNudgeQueue_SurvivesReincarnation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "nq02-send-bbbb-cccc-000000000001"
	const gen1 = "nq02-gen1-bbbb-cccc-000000000002"
	const gen2 = "nq02-gen2-bbbb-cccc-000000000003"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(gen1, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(gen1)
	f.HaveMember("team", sender)
	f.HaveMember("team", gen1)
	// gen1 is offline at send time.

	r := mustSend(t, f, sender, map[string]any{"to": gen1, "body": "survive the rotation"})
	require.True(t, r.Queued)
	nudge := fmt.Sprintf("new agent message #%d", r.ID)

	// Quiesce the send's async delivery worker WHILE gen1 is still offline, so
	// its drain runs as a no-op (no live pane) and the message stays
	// undelivered. Without this barrier the worker's single drain pass can be
	// scheduled after gen2 comes online below, re-resolve the agent's head to
	// gen2, and deliver the nudge itself — which leaves the explicit
	// FlushUndeliveredForTest with nothing to claim (count 0, not 1). That race
	// made this test ~9% flaky on CI (JOH-326). The sibling nudge-queue tests
	// barrier the same way after their sends.
	agentd.WaitForBackgroundForTest()

	// The recipient reincarnates: its actor's live conv rotates gen1 → gen2.
	_, err := db.RotateAgentConv(gen1, gen2, "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	// The fresh generation comes online under a new pane.
	const tmux2 = "tclaude-nq02-g2"
	f.HaveAliveSession(gen2, "spwn-nq02-g2", tmux2, "/tmp/work")

	// A drive of the recipient's NEW conv delivers the message queued against
	// the OLD one — it followed the agent.
	assert.Equal(t, 1, agentd.FlushUndeliveredForTest(gen2), "queued message follows the agent to its new generation")
	f.AssertSentContains(tmux2+":0.0", nudge, 2*time.Second)
	m, err := db.GetAgentMessage(r.ID)
	require.NoError(t, err)
	assert.False(t, m.DeliveredAt.IsZero(), "delivered to the live generation")
}

// TestNudgeQueue_PrevGenTargeting pins a message to a SPECIFIC past generation
// with `gen`, while an ordinary message to the same agent follows the head.
// The two land in different panes: the pinned one in the named generation's
// pane, the head-following one in the current generation's pane.
func TestNudgeQueue_PrevGenTargeting(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "nq03-send-bbbb-cccc-000000000001"
	const gen1 = "nq03-gen1-bbbb-cccc-000000000002"
	const gen2 = "nq03-gen2-bbbb-cccc-000000000003"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(gen1, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(gen1)
	f.HaveMember("team", sender)
	f.HaveMember("team", gen1)

	// Rotate gen1 → gen2 so gen1 is now a PREVIOUS generation; both panes are
	// alive (the prev-gen pane lingering is the case --gen exists for).
	_, err := db.RotateAgentConv(gen1, gen2, "reincarnate")
	require.NoError(t, err, "RotateAgentConv")
	const tmux1 = "tclaude-nq03-g1"
	const tmux2 = "tclaude-nq03-g2"
	f.HaveAliveSession(gen1, "spwn-nq03-g1", tmux1, "/tmp/work")
	f.HaveAliveSession(gen2, "spwn-nq03-g2", tmux2, "/tmp/work")

	// Ordinary send → follows the agent to its head (gen2).
	rHead := mustSend(t, f, sender, map[string]any{"to": gen2, "body": "to the head"})
	// Pinned send → the exact past generation gen1.
	rPin := mustSend(t, f, sender, map[string]any{"to": gen2, "gen": gen1, "body": "to the past"})
	require.True(t, rHead.Queued && rPin.Queued)

	agentd.WaitForBackgroundForTest()

	headNudge := fmt.Sprintf("new agent message #%d", rHead.ID)
	pinNudge := fmt.Sprintf("new agent message #%d", rPin.ID)
	f.AssertSentContains(tmux2+":0.0", headNudge, 2*time.Second)
	f.AssertSentContains(tmux1+":0.0", pinNudge, 2*time.Second)
	// And not crossed: the pinned message did not land in the head pane.
	assertNoSendKeys(t, f, tmux2+":0.0", pinNudge)
	assertNoSendKeys(t, f, tmux1+":0.0", headNudge)
}

// TestNudgeQueue_PrevGenRejectsForeignConv: `gen` must be a generation of the
// agent the target resolves to. A conv belonging to a DIFFERENT agent is a
// 400, so a caller can't smuggle a cross-agent conv past the agent-keyed route.
func TestNudgeQueue_PrevGenRejectsForeignConv(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	const sender = "nq04-send-bbbb-cccc-000000000001"
	const recipient = "nq04-recv-bbbb-cccc-000000000002"
	const stranger = "nq04-strg-bbbb-cccc-000000000003"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveConvWithTitle(stranger, "other")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recipient)
	f.HaveEnrolledAgent(stranger)
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveMember("team", stranger)

	rec := postMessage(t, f, sender, map[string]any{"to": recipient, "gen": stranger, "body": "x"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "gen of a foreign agent must be rejected; body=%s", rec.Body.String())
}

// mustSend POSTs a message and decodes the queue-centric response, asserting a
// 200. Shared by the nudge-queue tests.
func mustSend(t *testing.T, f *testharness.Flow, fromConv string, body map[string]any) sendRespView {
	t.Helper()
	rec := postMessage(t, f, fromConv, body)
	require.Equal(t, http.StatusOK, rec.Code, "send failed; body=%s", rec.Body.String())
	var r sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r), "decode send resp: %s", rec.Body.String())
	return r
}
