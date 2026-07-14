package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

// TestNudgeQueue_HungLivenessProbeDoesNotWedgeTarget reproduces TCL-281's
// signature: the first delivery worker gets stuck before it can atomically
// claim the message, every later enqueue coalesces behind running=true, and a
// daemon restart (which clears nudgeState) is the only recovery. The simulator
// hangs exactly the production boundary implicated by the live DB/log evidence
// — tmux has-session inside deliverablePane — while SQLite, routing, claiming,
// and the async dispatcher remain production code.
func TestNudgeQueue_HungLivenessProbeDoesNotWedgeTarget(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetTmuxCommandTimeoutForTest(time.Second))
	timeoutArmed, fireTimeout, restoreTimeout := agentd.ControlNextTmuxCommandTimeoutForTest()
	t.Cleanup(restoreTimeout)
	f.HaveGroup("team")
	const sender = "nq05-send-bbbb-cccc-000000000001"
	const recipient = "nq05-recv-bbbb-cccc-000000000002"
	const tmux = "tclaude-nq05-r"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recipient)
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveAliveSession(recipient, "spwn-nq05-r", tmux, "/tmp/work")

	// A local tmux command should finish in milliseconds. Sleeping much longer
	// than the delivery deadline models the anomalous client that parked the
	// live daemon's worker before ClaimAgentMessageNudge.
	f.World.Tmux.HangNextCommand("has-session", 30*time.Second)
	r1 := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "one"})
	select {
	case timeout := <-timeoutArmed:
		assert.Equal(t, time.Second, timeout)
	case <-time.After(10 * time.Second):
		t.Fatal("hung liveness probe did not start and arm its timeout")
	}

	// Arrive while the first pass is still in flight. The dispatcher marks the
	// target `again`; once the timed-out probe returns, that pass must release
	// and the re-armed pass must drain both rows without a daemon restart.
	r2 := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "two"})
	assert.Equal(t, 2, r2.Pending, "both messages are unclaimed while the probe is hung")
	fireTimeout()
	agentd.WaitForBackgroundForTest()
	assert.GreaterOrEqual(t, f.World.Tmux.CommandCount("has-session"), 2,
		"the timeout releases the first pass and the re-armed pass probes again")

	for _, id := range []int64{r1.ID, r2.ID} {
		m, err := db.GetAgentMessage(id)
		require.NoError(t, err)
		assert.False(t, m.DeliveredAt.IsZero(), "message #%d delivered after the probe timeout", id)
		f.AssertSentContains(tmux+":0.0", fmt.Sprintf("new agent message #%d", id), 2*time.Second)
	}
}

// TestNudgeQueue_HungSendKeysIsRetriedByReaper is the exact live TCL-281
// shape. The first row is durably claimed, its tmux send-keys never returns,
// and a second row arrives behind the target's running latch. The command
// deadline must release the latch, failed delivery must release only its own
// durable claim, the second row may proceed, and the regular reaper cadence
// must retry the first after its per-message backoff.
func TestNudgeQueue_HungSendKeysIsRetriedByReaper(t *testing.T) {
	f := newFlow(t)
	// One second, not 25ms: the healthy has-session probe BEFORE the hung
	// send-keys is a real subprocess spawn, and a loaded CI runner can blow
	// a 25ms deadline on that alone — delivery then fails into the minute
	// backoff below and send-keys is never reached (observed flake).
	t.Cleanup(agentd.SetTmuxCommandTimeoutForTest(time.Second))
	// Keep the coalesced `again` pass from retrying #1 before we can assert
	// the failed state. The test switches backoff off explicitly when it is
	// ready to drive the periodic reaper.
	t.Cleanup(agentd.SetNudgeRetryTimingForTest(time.Minute, time.Minute))
	f.HaveGroup("team")
	const sender = "nq06-send-bbbb-cccc-000000000001"
	const recipient = "nq06-recv-bbbb-cccc-000000000002"
	const tmux = "tclaude-nq06-r"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recipient)
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveAliveSession(recipient, "spwn-nq06-r", tmux, "/tmp/work")

	f.World.Tmux.HangNextCommand("send-keys", 30*time.Second)
	r1 := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "one"})
	require.Eventually(t, func() bool {
		return f.World.Tmux.CommandCount("send-keys") >= 1
	}, 5*time.Second, time.Millisecond, "first worker reaches the hung send-keys")
	r2 := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "two"})
	assert.Equal(t, 2, r2.Pending, "in-flight-but-unconfirmed #1 remains part of the durable queue")

	agentd.WaitForBackgroundForTest()
	m1, err := db.GetAgentMessage(r1.ID)
	require.NoError(t, err)
	assert.True(t, m1.DeliveredAt.IsZero(), "timed-out send is not falsely marked delivered")
	assert.True(t, m1.NudgeClaimedAt.IsZero(), "failed attempt released its claim")
	assert.Equal(t, 1, m1.NudgeAttempts, "failed attempt persisted for backoff")
	m2, err := db.GetAgentMessage(r2.ID)
	require.NoError(t, err)
	assert.False(t, m2.DeliveredAt.IsZero(), "later row is not wedged behind the failed attempt")
	f.AssertSentContains(tmux+":0.0", fmt.Sprintf("new agent message #%d", r2.ID), 2*time.Second)
	assertNoSendKeys(t, f, tmux+":0.0", fmt.Sprintf("new agent message #%d", r1.ID))

	// The recipient makes no request. Make the durable retry due, then drive
	// the existing 30s reaper path that regularly re-arms delivery.
	t.Cleanup(agentd.SetNudgeRetryTimingForTest(0, 0))
	agentd.RunReaperTickForTest(time.Now())
	agentd.WaitForBackgroundForTest()
	m1, err = db.GetAgentMessage(r1.ID)
	require.NoError(t, err)
	assert.False(t, m1.DeliveredAt.IsZero(), "reaper retry completes first delivery")
	assert.Equal(t, 2, m1.NudgeAttempts, "retry attempt is durable")
	f.AssertSentContains(tmux+":0.0", fmt.Sprintf("new agent message #%d", r1.ID), 2*time.Second)
}

// TestNudgeQueue_StaleUndeliveredWarnNamesTargetAndMessage pins the operator's
// observability requirement: an old durable row emits a WARN naming the queue
// key, oldest message id, and elapsed time, throttled per target.
func TestNudgeQueue_StaleUndeliveredWarnNamesTargetAndMessage(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetStaleNudgeTimingForTest(0, time.Hour))
	f.HaveGroup("team")
	const sender = "nq07-send-bbbb-cccc-000000000001"
	const recipient = "nq07-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recipient)
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveAliveSession(recipient, "spwn-nq07-r", "tclaude-nq07-r", "/tmp/work")
	f.SetSessionStatus(recipient, "awaiting_permission")

	r := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "held"})
	agentd.WaitForBackgroundForTest()

	prev := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	agentd.RunReaperTickForTest(time.Now())
	agentd.WaitForBackgroundForTest()
	agentd.RunReaperTickForTest(time.Now().Add(time.Second))
	agentd.WaitForBackgroundForTest()

	got := logs.String()
	assert.Contains(t, got, "level=WARN msg=\"nudge delivery queue stuck\"")
	assert.Contains(t, got, "target=agent:")
	assert.Contains(t, got, fmt.Sprintf("msg_id=%d", r.ID))
	assert.Contains(t, got, "elapsed=")
	assert.Equal(t, 1, strings.Count(got, "nudge delivery queue stuck"), "warning throttled per target")
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

	// Drain the async per-target worker the send armed (enqueueDeliveryForConv)
	// while gen1 is still OFFLINE, so it HOLDS the backlog — the canDeliver gate
	// runs before the claim — instead of racing the explicit drain below. Without
	// this, a late-scheduled worker can run after gen2 comes online, resolve the
	// agent head to gen2, and win the atomic ClaimAgentMessageNudge first,
	// leaving the synchronous drain to claim 0 (the macOS-CI flake). Mirrors
	// TestNudgeQueue_HoldsThenDeliversBacklog's pre-online WaitForBackgroundForTest.
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

// TestNudgeQueue_RetiredTargetCancelsQueuedNudges pins the retired-target
// cleanup: a message queued for an agent that is then retired must be
// CANCELLED by the reaper sweep — one WARN naming the message and reason,
// no "stuck" watchdog noise on later ticks (the operator's every-boot
// warning spam) — while staying unread in the inbox. Reinstating the agent
// revives the queue and delivery completes once the agent comes online.
func TestNudgeQueue_RetiredTargetCancelsQueuedNudges(t *testing.T) {
	f := newFlow(t)
	// Zero staleness threshold: any queue the cancel sweep left behind would
	// warn as stuck on the very first tick, so the "no stuck warning" assert
	// below is meaningful.
	t.Cleanup(agentd.SetStaleNudgeTimingForTest(0, time.Hour))
	f.HaveGroup("team")
	const sender = "nq08-send-bbbb-cccc-000000000001"
	const recipient = "nq08-recv-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(sender, "po")
	f.HaveConvWithTitle(recipient, "worker")
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recipient)
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	// recipient is OFFLINE — the message queues durably.

	r := mustSend(t, f, sender, map[string]any{"to": recipient, "body": "orphaned"})
	require.True(t, r.Queued)
	agentd.WaitForBackgroundForTest()

	agentID, err := db.AgentIDForConv(recipient)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	retired, err := db.RetireAgentByID(agentID, "human", "test cleanup")
	require.NoError(t, err)
	require.True(t, retired)

	prev := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	agentd.RunReaperTickForTest(time.Now())
	agentd.WaitForBackgroundForTest()
	agentd.RunReaperTickForTest(time.Now().Add(time.Second))
	agentd.WaitForBackgroundForTest()

	got := logs.String()
	assert.Contains(t, got, "level=WARN msg=\"nudge delivery cancelled; target unavailable\"")
	assert.Contains(t, got, fmt.Sprintf("msg_id=%d", r.ID))
	assert.Contains(t, got, "reason=\"target agent retired\"")
	assert.Equal(t, 1, strings.Count(got, "nudge delivery cancelled"), "cancellation logged exactly once")
	assert.NotContains(t, got, "nudge delivery queue stuck", "cancelled queue no longer warns as stuck")

	// Cancelled ≠ delivered/read: the message is still readable in the inbox,
	// only its nudge delivery is abandoned.
	m, err := db.GetAgentMessage(r.ID)
	require.NoError(t, err)
	assert.True(t, m.DeliveredAt.IsZero(), "cancel is not a delivery stamp")
	assert.True(t, m.ReadAt.IsZero(), "cancel is not a read stamp")
	assert.False(t, m.NudgeCancelledAt.IsZero(), "cancellation is durable")
	assert.Equal(t, "target agent retired", m.NudgeCancelReason)

	// Soft retire stays reversible: reinstating clears the cancellation and
	// the queue delivers once the agent comes back online.
	reinstated, err := db.ReinstateAgentByID(agentID)
	require.NoError(t, err)
	require.True(t, reinstated)
	m, err = db.GetAgentMessage(r.ID)
	require.NoError(t, err)
	assert.True(t, m.NudgeCancelledAt.IsZero(), "reinstate revives the queued nudge")
	assert.Empty(t, m.NudgeCancelReason)

	const tmux = "tclaude-nq08-r"
	f.HaveAliveSession(recipient, "spwn-nq08-r", tmux, "/tmp/work")
	assert.Equal(t, 1, agentd.FlushUndeliveredForTest(recipient), "revived message delivers")
	f.AssertSentContains(tmux+":0.0", fmt.Sprintf("new agent message #%d", r.ID), 2*time.Second)
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
