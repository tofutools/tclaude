package agentd

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// unreadReminderInterval is how long a delivered-but-unread message waits
// before the recipient is re-nudged, and the cadence of every re-nudge after
// that. The driving requirement is "remind every 10 minutes or so" with a
// first reminder no sooner than ~10 minutes after delivery — so the same
// constant governs both the initial delay and the repeat gap.
var unreadReminderInterval = 10 * time.Minute

// unreadReminderSweepInterval is how often the sweep wakes to look for due
// reminders. It must be well under unreadReminderInterval so a reminder fires
// within a minute or so of becoming due (rather than up to a full interval
// late). 1 minute keeps the actual cadence close to "every 10 minutes" while
// costing one cheap SELECT per minute on a daemon that is otherwise idle.
const unreadReminderSweepInterval = 1 * time.Minute

// unreadReminderState tracks, per recipient conv, when that agent was last
// reminded about its unread mail. It is the in-memory cadence clock: a
// recipient is due again unreadReminderInterval after its last reminder (or,
// before the first reminder, after its oldest unread message was delivered —
// floored at `epoch`, see below).
//
// In-memory (not a DB column) is a deliberate, low-risk choice: a reminder is
// best-effort UX, the map is tiny (one entry per agent with outstanding mail),
// and the worst a daemon restart can do is fire one extra reminder per agent
// — which only lands if the message is still unread AND the agent is online,
// idle and not retired. Surviving a restart precisely is not worth a schema
// migration here.
type unreadReminderState struct {
	mu         sync.Mutex
	remindedAt map[string]time.Time // to_conv → last reminder time
	// epoch is the floor a never-yet-reminded conv's reference is clamped to:
	// the time the sweep started observing. A message delivered before this
	// process started is therefore not due until a full interval AFTER
	// startup, so a daemon restart can't re-nudge a backlog of long-delivered
	// mail on its first ticks. Zero ⇒ no floor (the tests' default).
	epoch time.Time
}

func newUnreadReminderState() *unreadReminderState {
	return &unreadReminderState{remindedAt: map[string]time.Time{}}
}

func (st *unreadReminderState) setEpoch(t time.Time) {
	st.mu.Lock()
	st.epoch = t
	st.mu.Unlock()
}

// the daemon's single sweep state, shared by the goroutine and the ForTest
// entry point.
var unreadReminders = newUnreadReminderState()

// startUnreadReminderSweep spins up the unread-message reminder loop in its
// own goroutine. It ticks every unreadReminderSweepInterval, re-nudging any
// online, idle, non-retired agent that has had a delivered message sit unread
// for unreadReminderInterval. Returns when stop is closed (the daemon-wide
// quit channel) — it shares cronStop with the other housekeeping sweeps.
//
// The sweep's start time is recorded as the cadence-clock epoch: a message
// delivered before this process started is not due until a full interval
// AFTER startup, so a daemon restart never re-nudges a backlog of long-
// delivered mail on its first tick. (Messages delivered while the daemon runs
// are unaffected — their own delivery time already follows the epoch.) Unlike
// the cron scheduler, the first tick is therefore not fired immediately; one
// sweep interval later is soon enough and keeps startup uncontended.
func startUnreadReminderSweep(stop <-chan struct{}) {
	unreadReminders.setEpoch(time.Now())
	go func() {
		t := time.NewTicker(unreadReminderSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runUnreadReminderTick(now)
			}
		}
	}()
}

// runUnreadReminderTick is a single sweep: it lists every delivered-but-unread
// message, groups them by recipient, and for each recipient whose reminder is
// due, re-nudges the live pane — unless the agent is offline, retired, or has
// tmux input effectively blocked (a permission prompt or elicitation dialog is
// up), in which case the recipient is skipped without advancing its clock, so
// it is reconsidered next tick.
func runUnreadReminderTick(now time.Time) {
	runUnreadReminderTickWith(now, unreadReminders)
}

// reminderTargetConv returns the conv whose pane an unread message should be
// re-nudged into: the recipient agent's CURRENT head generation when the
// message is head-following (to_agent set), else the recorded to_conv. This
// keeps the reminder correct across a reincarnate/`/clear` that rotated the
// recipient's conv-id after delivery (JOH-310).
func reminderTargetConv(m *db.AgentMessage) string {
	if m.ToAgent != "" {
		if head, err := db.CurrentConvForAgent(m.ToAgent); err == nil && head != "" {
			return head
		}
	}
	return m.ToConv
}

// runUnreadReminderTickWith is the testable core: the state is passed in so a
// test can drive the cadence with a fresh clock.
func runUnreadReminderTickWith(now time.Time, st *unreadReminderState) {
	msgs, err := db.ListDeliveredUnreadAgentMessages()
	if err != nil {
		slog.Warn("unread-reminder: list failed", "error", err)
		return
	}

	// Group by recipient's LIVE conv, preserving id order (oldest first) within
	// each. We key on the agent's current head generation, not the recorded
	// to_conv (JOH-310): a head-following message delivered across a
	// reincarnate/`/clear` keeps to_conv = the old, now-dead generation, so
	// grouping by to_conv would target a dead pane and the agent would never be
	// reminded. reminderTargetConv resolves to_agent → current head; non-actor
	// mail falls back to to_conv unchanged.
	byConv := map[string][]*db.AgentMessage{}
	order := []string{}
	for _, m := range msgs {
		conv := reminderTargetConv(m)
		if _, seen := byConv[conv]; !seen {
			order = append(order, conv)
		}
		byConv[conv] = append(byConv[conv], m)
	}

	// Decide which recipients are due, and prune clock entries for recipients
	// with nothing outstanding (mail read or deleted since last tick). Both
	// touch the cadence map, so do them under the lock — then release it
	// before any DB / tmux I/O, so the map is never held across blocking work.
	st.mu.Lock()
	for conv := range st.remindedAt {
		if _, still := byConv[conv]; !still {
			delete(st.remindedAt, conv)
		}
	}
	due := make([]string, 0, len(order))
	for _, conv := range order {
		if st.dueLocked(conv, byConv[conv], now) {
			due = append(due, conv)
		}
	}
	st.mu.Unlock()

	for _, conv := range due {
		// Gate on the recipient actually being a live, idle, non-retired agent
		// before we touch its pane.
		sess := pickAliveSession(conv)
		if sess == nil {
			continue // offline — nothing to nudge; reconsider next tick
		}
		if isTmuxInputBlocked(sess.Status) {
			continue // permission/elicitation dialog up — noop, retry next tick
		}
		if live, err := db.IsLiveAgentConv(conv); err != nil || !live {
			// Retired, a superseded predecessor generation, or not an agent at
			// all. Per the brief, reminders apply only to live agents. A real
			// DB error is logged (and retried next tick) rather than buried.
			if err != nil {
				slog.Debug("unread-reminder: liveness check failed", "error", err, "conv", conv)
			}
			continue
		}
		if err := injectTextAndSubmit(sess.TmuxSession+":0.0", unreadReminderText(byConv[conv])); err != nil {
			slog.Warn("unread-reminder: inject failed",
				"error", err, "tmux", sess.TmuxSession, "conv", conv)
			continue // leave the clock unadvanced so we retry next tick
		}
		st.mu.Lock()
		st.remindedAt[conv] = now
		st.mu.Unlock()
		slog.Info("unread-reminder: nudged", "conv", conv, "unread", len(byConv[conv]))
	}
}

// dueLocked reports whether `conv` is due for a reminder at `now`. The caller
// holds st.mu. The reference point is the last time we reminded this conv, or
// — before any reminder — the earliest delivery among its unread messages,
// floored at st.epoch so a freshly-restarted daemon doesn't treat a long-
// delivered backlog as instantly due.
func (st *unreadReminderState) dueLocked(conv string, list []*db.AgentMessage, now time.Time) bool {
	ref, ok := st.remindedAt[conv]
	if !ok {
		ref = earliestDelivered(list)
		if ref.IsZero() {
			return false // no usable delivery timestamp; skip defensively
		}
		if !st.epoch.IsZero() && ref.Before(st.epoch) {
			ref = st.epoch
		}
	}
	return !now.Before(ref.Add(unreadReminderInterval))
}

// earliestDelivered returns the oldest delivered_at across the messages. id
// order is roughly delivery order, but a message first delivered late via the
// flush path can carry a low id, so we scan rather than trust list[0].
func earliestDelivered(list []*db.AgentMessage) time.Time {
	var oldest time.Time
	for _, m := range list {
		if m.DeliveredAt.IsZero() {
			continue
		}
		if oldest.IsZero() || m.DeliveredAt.Before(oldest) {
			oldest = m.DeliveredAt
		}
	}
	return oldest
}

// isTmuxInputBlocked reports whether a pane in this status is showing a modal
// that "owns" the keyboard — a permission prompt or an elicitation dialog —
// so typing a reminder into it would answer the dialog instead of queueing a
// prompt. Working/idle panes are fine: a nudge queues behind an in-flight turn
// or submits as a fresh prompt, exactly as first delivery does.
func isTmuxInputBlocked(status string) bool {
	return status == session.StatusAwaitingPermission ||
		status == session.StatusAwaitingInput
}

// unreadReminderText builds the bracketed reminder injected into the
// recipient's pane. A single outstanding message names its sender (stable
// agent_id, like messageNudgeText) and points at `inbox read`; several
// collapse to a count + oldest id pointing at `inbox ls`, so a backlog is one
// terse line rather than a flood. list is id-ordered, so list[0] is the
// lowest-id (oldest-sent) unread message.
func unreadReminderText(list []*db.AgentMessage) string {
	if len(list) == 1 {
		m := list[0]
		if sender := agent.MessageSenderLabel(m.FromConv, m.FromAgent); sender != "" {
			return fmt.Sprintf(
				"[system: reminder — unread agent message #%d from %s is still unread. read it with: tclaude agent inbox read %d]",
				m.ID, sender, m.ID)
		}
		return fmt.Sprintf(
			"[system: reminder — agent message #%d is still unread. read it with: tclaude agent inbox read %d]",
			m.ID, m.ID)
	}
	return fmt.Sprintf(
		"[system: reminder — you have %d unread agent messages (oldest #%d). list them with: tclaude agent inbox ls]",
		len(list), list[0].ID)
}
