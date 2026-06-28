package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests pin the unread-message reminder sweep: a delivered-but-unread
// message re-nudges its recipient every unreadReminderInterval, but only while
// the recipient is an online, idle, non-retired agent. A pane blocked on a
// permission / elicitation dialog is a noop until it clears. The cadence clock
// is driven with an explicit `now` so the 10-minute interval is exercised
// without sleeping.

const (
	urSender    = "urem-send-bbbb-cccc-000000000001"
	urRecipient = "urem-recv-bbbb-cccc-000000000002"
	urLabel     = "spwn-urem-r"
	urTmux      = "tclaude-spwn-urem-r"
	urTarget    = urTmux + ":0.0"
)

// haveUnreadMessage stands up a group with an alive recipient, sends it a
// message, drains the async delivery worker, and returns the flow plus the
// delivered message id. The message is then delivered-but-unread — exactly the
// state the reminder sweep acts on.
func haveUnreadMessage(t *testing.T, body string) (*testharness.Flow, int64) {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("team")
	f.HaveConvWithTitle(urSender, "po-coordinator")
	f.HaveMember("team", urSender)
	f.HaveMember("team", urRecipient)
	f.HaveAliveSession(urRecipient, urLabel, urTmux, "/tmp/work")

	rec := postMessage(t, f, urSender, map[string]any{"to": urRecipient, "body": body})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp sendRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.Queued, "alive recipient: message queued for async delivery")
	require.NotZero(t, resp.ID)
	// Delivery is async now (JOH-310): drain the worker so the alive recipient
	// is actually nudged and delivered_at is stamped — the precondition the
	// unread-reminder sweep keys on (delivered-but-unread).
	agentd.WaitForBackgroundForTest()
	msg, err := db.GetAgentMessage(resp.ID)
	require.NoError(t, err)
	require.False(t, msg.DeliveredAt.IsZero(), "async worker delivered to the alive recipient")
	return f, resp.ID
}

// assertNoReminder fails if any reminder line was injected into the target.
// The sweep injects synchronously, so the Sent log is complete on return.
func assertNoReminder(t *testing.T, f *testharness.Flow) {
	t.Helper()
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == urTarget && strings.Contains(sk.Text, "reminder —") {
			t.Fatalf("expected no reminder to %s, got %q", urTarget, sk.Text)
		}
	}
}

// setRecipientStatus rewrites the recipient's live session row status — the
// signal the sweep reads to decide whether the pane's input is blocked.
func setRecipientStatus(t *testing.T, status string) {
	t.Helper()
	rows, err := db.FindSessionsByConvID(urRecipient)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	r := rows[0]
	r.Status = status
	require.NoError(t, db.SaveSession(r))
}

// TestUnreadReminder_FiresAfterInterval is the happy path: a delivered message
// left unread for one interval re-nudges the online, idle recipient.
func TestUnreadReminder_FiresAfterInterval(t *testing.T) {
	f, _ := haveUnreadMessage(t, "please review")
	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now()

	// Before the interval: nothing fires.
	agentd.RunUnreadReminderTickForTest(base.Add(5*time.Minute), st)
	assertNoReminder(t, f)

	// After the interval: a single-message reminder pointing at inbox read.
	agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)
	f.AssertSentContains(urTarget, "reminder —", time.Second)
	f.AssertSentContains(urTarget, "inbox read", time.Second)
}

// TestUnreadReminder_RestartFloorDefersBacklog pins the restart-herd guard: a
// message delivered BEFORE the daemon (here, the sweep epoch) started is not
// due until a full interval after startup — not a full interval after its
// original delivery — so a restart can't re-nudge an old backlog on its first
// tick. The epoch is set 5 min after delivery; without the floor the message
// would be due at delivery+10, but the floor pushes it to epoch+10.
func TestUnreadReminder_RestartFloorDefersBacklog(t *testing.T) {
	f, _ := haveUnreadMessage(t, "delivered before restart")
	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now() // ~ delivery time
	agentd.SeedUnreadReminderEpochForTest(st, base.Add(5*time.Minute))

	// base+12m is past delivery+10m (would fire without the floor) but short
	// of epoch+10m = base+15m, so the floor defers it.
	agentd.RunUnreadReminderTickForTest(base.Add(12*time.Minute), st)
	assertNoReminder(t, f)

	// base+16m is past epoch+10m → the deferred reminder fires.
	agentd.RunUnreadReminderTickForTest(base.Add(16*time.Minute), st)
	f.AssertSentContains(urTarget, "reminder —", time.Second)
}

// TestUnreadReminder_RepeatsEveryInterval pins the cadence: after the first
// reminder, the recipient is re-nudged only once per interval, not every tick.
func TestUnreadReminder_RepeatsEveryInterval(t *testing.T) {
	f, _ := haveUnreadMessage(t, "still waiting")
	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now()

	agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)
	first := remindersTo(f, urTarget)
	require.Equal(t, 1, first, "one reminder after the first interval")

	// A tick well within the next interval must NOT re-nudge.
	agentd.RunUnreadReminderTickForTest(base.Add(15*time.Minute), st)
	require.Equal(t, 1, remindersTo(f, urTarget), "no second reminder within the interval")

	// A tick past the next interval boundary fires the second reminder.
	agentd.RunUnreadReminderTickForTest(base.Add(22*time.Minute), st)
	require.Equal(t, 2, remindersTo(f, urTarget), "second reminder after the next interval")
}

// TestUnreadReminder_NoopWhenInputBlocked covers the brief's key carve-out: a
// pane blocked on a permission / elicitation dialog is skipped WITHOUT
// advancing its clock, so it fires the moment the dialog clears.
func TestUnreadReminder_NoopWhenInputBlocked(t *testing.T) {
	for _, status := range []string{"awaiting_permission", "awaiting_input"} {
		t.Run(status, func(t *testing.T) {
			f, _ := haveUnreadMessage(t, "blocked case")
			st := agentd.NewUnreadReminderStateForTest()
			base := time.Now()

			setRecipientStatus(t, status)
			agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)
			assertNoReminder(t, f) // blocked → noop, clock untouched

			// Dialog clears → next tick fires immediately (still overdue).
			setRecipientStatus(t, "idle")
			agentd.RunUnreadReminderTickForTest(base.Add(12*time.Minute), st)
			f.AssertSentContains(urTarget, "reminder —", time.Second)
		})
	}
}

// TestUnreadReminder_SkipsRetiredAgent pins "online agents that are not
// retired": a retired recipient is never reminded, even with unread mail.
func TestUnreadReminder_SkipsRetiredAgent(t *testing.T) {
	f, _ := haveUnreadMessage(t, "to a soon-retired agent")
	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now()

	// Retire the recipient's actor; its pane stays alive but it is no longer
	// a live agent.
	retired, err := db.RetireAgent(urRecipient, "", "test")
	require.NoError(t, err)
	require.True(t, retired)
	agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)
	assertNoReminder(t, f)
}

// TestUnreadReminder_SkipsOfflineAgent pins "online agents": an offline
// recipient is skipped (there is no live pane to nudge).
func TestUnreadReminder_SkipsOfflineAgent(t *testing.T) {
	f, _ := haveUnreadMessage(t, "to a soon-offline agent")
	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now()

	f.MarkOffline(urTmux)
	agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)
	assertNoReminder(t, f)
}

// TestUnreadReminder_StopsOnceRead confirms reading the message ends the
// reminders — read_at is set, so the sweep no longer sees it.
func TestUnreadReminder_StopsOnceRead(t *testing.T) {
	f, id := haveUnreadMessage(t, "will be read")
	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now()

	require.NoError(t, db.MarkAgentMessageRead(id))
	agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)
	assertNoReminder(t, f)
}

// TestUnreadReminder_AggregatesBacklog confirms several unread messages
// collapse to a single count-bearing reminder rather than one per message.
func TestUnreadReminder_AggregatesBacklog(t *testing.T) {
	f, _ := haveUnreadMessage(t, "first")
	// A second message to the same recipient from the same sender.
	rec := postMessage(t, f, urSender, map[string]any{"to": urRecipient, "body": "second"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest() // deliver the second message too (async)

	st := agentd.NewUnreadReminderStateForTest()
	base := time.Now()
	agentd.RunUnreadReminderTickForTest(base.Add(11*time.Minute), st)

	f.AssertSentContains(urTarget, "2 unread agent messages", time.Second)
	require.Equal(t, 1, remindersTo(f, urTarget), "a backlog is one aggregate reminder, not one per message")
}

// remindersTo counts reminder lines injected into the target so far.
func remindersTo(f *testharness.Flow, target string) int {
	n := 0
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target && strings.Contains(sk.Text, "reminder —") {
			n++
		}
	}
	return n
}
