package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// sessionReaperInterval is how often the reaper sweeps for sessions
// whose process has gone. 30s matches the cron scheduler's cadence —
// "offline" is a coarse state, a sub-minute lag is imperceptible, and a
// tighter loop would just burn `tmux has-session` calls.
const sessionReaperInterval = 30 * time.Second

// sessionReaperGracePeriod exempts a freshly-created session row from
// being reaped. A spawn writes the SessionRow around the same moment
// its tmux session comes up; without this window a sweep that lands
// mid-spawn could mark a starting agent "exited" before its first hook
// fires. A genuinely short-lived session is simply reaped a tick or two
// later instead — at the cost of no offline notification for a sub-90s
// life, which is acceptable (such a notification would be noise).
const sessionReaperGracePeriod = 90 * time.Second

// reaperNotify is the offline-transition notification seam. Production
// routes it through notify.OnStateTransition; tests swap in a recorder.
// Mirrors the flushSender injection pattern in flush.go.
type reaperNotify func(st *session.SessionState, prevStatus string)

const unexpectedExitReason = "unexpected"

// sessionReaper marks sessions whose tmux session and process are both
// gone as "exited" in the DB, and fires an offline notification on the
// alive→dead transition.
//
// It carries the set of session IDs seen alive on the previous tick so
// a notification only fires for a transition it personally witnessed.
// The first tick after construction merely seeds that set: a fresh
// daemon has no prior observation, so every already-dead row it finds
// is a pre-existing corpse — reaped silently, never notified. Without
// this, a daemon restart would fire a notification storm for the whole
// backlog of stale rows.
type sessionReaper struct {
	aliveLastTick map[string]bool
	seeded        bool
	grace         time.Duration
	notify        reaperNotify
}

func newSessionReaper() *sessionReaper {
	return &sessionReaper{
		aliveLastTick: map[string]bool{},
		grace:         sessionReaperGracePeriod,
		notify:        defaultReaperNotify,
	}
}

// defaultReaperNotify routes an offline transition through the shared
// notification path. notify.OnStateTransition no-ops when notifications
// are disabled (the default). prevStatus here is never "exited" — tick
// skips already-exited rows before capturing it — so the reaper itself
// cannot produce an exited→exited repeat; the reverse race (a late
// SessionEnd hook landing after the reaper already stamped exited) is
// suppressed by OnStateTransition's self-transition guard.
func defaultReaperNotify(st *session.SessionState, prevStatus string) {
	notify.OnStateTransition(st.ID, st.ConvID, prevStatus, session.StatusExited, st.Cwd, agent.FreshTitle(st.ConvID), st.Harness)
}

// RunReaperTickForTest runs a single session-reaper sweep synchronously and
// returns the number of sessions reaped. It exists so a flow test can drive
// the reaper's resume-delivery backstop (maybeFlushUndelivered per alive
// session) without standing up the 30s ticker goroutine. Production drives
// tick from startSessionReaper. The flush it triggers is still
// goBackground + debounced, so drain with WaitForBackgroundForTest before
// asserting delivery. Not reachable from production — a sanctioned …ForTest
// entry into the real reaper path.
func RunReaperTickForTest(now time.Time) int {
	return newSessionReaper().tick(now)
}

// startSessionReaper runs the reaper in its own goroutine, ticking
// every sessionReaperInterval until stop is closed (the daemon-wide
// quit channel). The first sweep fires immediately so a restart picks
// up dead rows without waiting a full interval — see tick for why that
// first sweep is notification-silent.
func startSessionReaper(stop <-chan struct{}) {
	go func() {
		r := newSessionReaper()
		r.tick(time.Now())
		t := time.NewTicker(sessionReaperInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				r.tick(now)
			}
		}
	}()
}

// enrollOnlineSession records a live session's conversation as an agent
// so every terminal-launched conversation — `tclaude conv new`, a plain
// reattached session, anything with a running pane — surfaces on the
// dashboard (in its group, or the virtual "Ungrouped" group) the same
// way a web-UI spawn does, without first having to be put in a group,
// granted a permission, or run a `tclaude agent` command.
//
// It is called from every reaper tick, so enrollment is continuous and
// self-healing: a conv that comes online after daemon startup is picked
// up on the next sweep (≤ one reaper interval), not only at boot. The
// reaper's first tick fires immediately at startup, so this also closes
// the gap the v29→v30 migration can't — it backfills agents from the
// durable agentic tables (groups, grants, succession, …) but cannot
// tmux-probe an online-but-otherwise-unrecorded session from inside a
// SQL migration.
//
// Idempotent and retirement-safe: EnsureAgentForConv mints / links an actor
// only when the conv is not already known, so a conv that already has an actor
// is left untouched and one the human deliberately retired is never un-retired
// — a retired agent whose pane is still alive stays retired.
func enrollOnlineSession(st *session.SessionState) {
	if st.ConvID == "" {
		return
	}
	if _, _, err := db.EnsureAgentForConv(st.ConvID, "online-reconcile"); err != nil {
		slog.Warn("reaper: ensure online session actor failed", "conv", st.ConvID, "error", err)
	}
}

// tick is one reaper sweep. For every non-exited session it refreshes
// liveness via session.RefreshSessionStatus — the exact tmux→PID check
// `session ls` derives on read, so the persisted status column cannot
// disagree with the terminal view — and CAS-writes "exited" on the ones
// that died. Returns the number of sessions reaped this sweep.
func (r *sessionReaper) tick(now time.Time) (reaped int) {
	// Queue health runs first and is DB-only, so the target/msg/elapsed WARNs
	// still emit even if the subsequent tmux sweep blocks or fails. Order
	// within the trio matters: lease recovery first, so a row whose orphaned
	// claim just expired becomes cancellable THIS tick; then the cancel sweep,
	// so a queue whose target is retired/deleted is cancelled (with its own
	// one-time WARN) before the watchdog could report it as stuck. A row whose
	// claim is still live (in-flight delivery) stays warnable — that is a real
	// in-flight incident, not an orphan.
	releaseExpiredNudgeClaims(now)
	cancelUnavailableNudgeTargets(now)
	warnStaleNudgeQueues(now)
	states, err := session.ListSessionStates()
	if err != nil {
		slog.Warn("reaper: list sessions failed", "error", err)
		return 0
	}
	aliveNow := make(map[string]bool, len(states))
	for _, st := range states {
		if st.Status == session.StatusExited {
			continue // already exited — nothing to reap
		}
		prevStatus := st.Status
		prevUpdated := st.Updated
		session.RefreshSessionStatus(st) // tmux→PID; sets exited iff both gone
		if st.Status != session.StatusExited {
			aliveNow[st.ID] = true
			// A live session is a running agent — enroll it so every
			// terminal-launched conversation surfaces on the dashboard
			// like a web-UI spawn does. See enrollOnlineSession.
			enrollOnlineSession(st)
			// Backstop delivery for this alive agent: flush any undelivered
			// mail it has queued. This was added for the mail-hold release
			// (a message held while the agent was blocked on a human is
			// delivered within ~one reaper interval of it resuming, even if
			// the agent makes no `tclaude agent` call of its own), but it is
			// not limited to that — it also proactively delivers ORDINARY
			// offline-queued mail that previously waited for the recipient's
			// next request. flush() self-gates (no-ops for an empty inbox or
			// a recipient still awaiting human input) and maybeFlushUndelivered
			// debounces per-conv, so this is a cheap, idempotent complement to
			// the request-driven flush in the identity middleware.
			maybeFlushUndelivered(st.ConvID)
			continue
		}
		// Looks dead. A row created within the grace window may just be
		// mid-spawn (tmux session not up yet) — leave it for a later
		// tick rather than reap a starting agent.
		if !st.Created.IsZero() && now.Sub(st.Created) < r.grace {
			continue
		}
		ok, err := db.MarkSessionExitedIfUnchanged(st.ID, prevStatus, prevUpdated, reaperFallbackExitReason(st.Harness))
		if err != nil {
			slog.Warn("reaper: mark exited failed", "session", st.ID, "error", err)
			continue
		}
		if !ok {
			// The row changed under us — almost always a resume that
			// flipped status back. Leave it; re-evaluated next sweep.
			continue
		}
		reaped++
		slog.Info("reaper: session exited",
			"session", st.ID, "conv", st.ConvID, "prev_status", prevStatus)
		// Notify only for a transition we witnessed: the session must
		// have been alive on the previous tick, and there must have
		// been a previous tick (seeded).
		if r.seeded && r.aliveLastTick[st.ID] {
			r.notify(st, prevStatus)
		}
	}
	r.aliveLastTick = aliveNow
	r.seeded = true
	return reaped
}

// reaperFallbackExitReason classifies a dead session when no explicit
// exit reason was recorded before the reaper observed it gone. Claude
// Code has a SessionEnd hook for graceful exits, so missing that hook
// means an abnormal death. Codex does not have an equivalent reliable
// end hook, and a user closing the pane is indistinguishable from a
// plain terminal exit here, so leave it reasonless unless another path
// recorded a reason first. A plain shell session has no hook at all —
// a clean exit (Ctrl-D, `exit`) is the normal way to end it — so it
// gets the same reasonless treatment; stamping "unexpected" would turn
// every deliberate shell exit into a spurious "Exited" banner.
func reaperFallbackExitReason(h string) string {
	if h == harness.CodexName || h == session.ShellHarnessName {
		return ""
	}
	return unexpectedExitReason
}
