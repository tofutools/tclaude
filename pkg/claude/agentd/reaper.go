package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
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
// are disabled (the default) and dedupes via the per-session cooldown —
// so a clean exit already announced by the SessionEnd hook is not
// double-notified when the reaper's next sweep observes the same exit.
func defaultReaperNotify(st *session.SessionState, prevStatus string) {
	notify.OnStateTransition(st.ID, prevStatus, session.StatusExited, st.Cwd, agent.FreshTitle(st.ConvID))
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

// reconcileOnlineEnrollment enrolls every conv with a live tmux
// session at daemon startup. The v29→v30 migration backfills agents
// from the durable agentic tables (groups, grants, succession, …), but
// a conv that was merely online and otherwise unrecorded — an
// ungrouped agent predating the enrollment feature — can't be
// tmux-probed from inside a SQL migration. This one-shot sweep closes
// that gap. Idempotent: EnrollAgent is INSERT OR IGNORE, so a conv the
// migration already enrolled (or one already retired) is left alone.
func reconcileOnlineEnrollment() {
	sessions, err := db.ListSessions()
	if err != nil {
		slog.Warn("enrollment reconcile: list sessions failed", "error", err)
		return
	}
	enrolled := 0
	for _, s := range sessions {
		if s.ConvID == "" || s.TmuxSession == "" {
			continue
		}
		if !session.IsTmuxSessionAlive(s.TmuxSession) {
			continue
		}
		if err := db.EnrollAgent(s.ConvID, "online-reconcile"); err != nil {
			slog.Warn("enrollment reconcile: enroll failed", "conv", s.ConvID, "error", err)
			continue
		}
		enrolled++
	}
	if enrolled > 0 {
		slog.Info("enrollment reconcile: enrolled online agents", "count", enrolled)
	}
}

// tick is one reaper sweep. For every non-exited session it refreshes
// liveness via session.RefreshSessionStatus — the exact tmux→PID check
// `session ls` derives on read, so the persisted status column cannot
// disagree with the terminal view — and CAS-writes "exited" on the ones
// that died. Returns the number of sessions reaped this sweep.
func (r *sessionReaper) tick(now time.Time) (reaped int) {
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
			continue
		}
		// Looks dead. A row created within the grace window may just be
		// mid-spawn (tmux session not up yet) — leave it for a later
		// tick rather than reap a starting agent.
		if !st.Created.IsZero() && now.Sub(st.Created) < r.grace {
			continue
		}
		ok, err := db.MarkSessionExitedIfUnchanged(st.ID, prevStatus, prevUpdated)
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
	if reaped > 0 {
		// Reaper writes originate in this goroutine, so they bypass
		// the HTTP-mux publishOnSuccessfulWrite seam. Nudge the
		// dashboard SSE broadcaster directly so an agent flipping
		// alive→exited surfaces without waiting for the next 2s
		// poll.
		dashboardEvents.Publish()
	}
	return reaped
}
