package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Background-work liveness reconcile.
//
// Claude Code fires a PostToolUse hook when a `Bash` call is launched with
// run_in_background — or a `Monitor` watch is started — and NO hook at all
// when either ends. The ledgers the hooks feed (db.BgShellSet,
// db.MonitorSet) can therefore only grow on their own, and the moment
// their counts are read is exactly the idle window in which stale entries
// are most likely — so hook-only badges would show ghosts precisely when
// they are looked at. That is why the previous decision was "no
// background-shell count" at all.
//
// This is the missing half: at dashboard read time, an agent's ledgers are
// re-derived from the processes actually running below it. It runs on the
// read path rather than a polling loop for the same reason
// refreshCodexContextSnapshotOnRead does — the work is only needed when
// somebody is looking, and the dashboard poll already provides the cadence.
//
// Both ledgers are reconciled in ONE pass, against ONE process list, with
// one claim per process. See session.ReconcileBackground for why splitting
// them would let a shell and a monitor with similar commands claim the
// same process and so retire neither.

const (
	// bgShellReconcileMinInterval throttles the /proc (or `ps`) scan per
	// session. The dashboard polls every agent on every tick, so without
	// this a busy roster would re-walk the whole process table many times
	// a second. One second is well below human perception of the badge
	// clearing and keeps the scan off the hot path.
	bgShellReconcileMinInterval = time.Second
	// bgShellCacheTTL bounds how long a session's cached verdict is kept
	// after its last read, so the map does not grow with every agent that
	// has ever been rendered.
	bgShellCacheTTL = 5 * time.Minute
	// bgShellRefreshAfter is how stale a ledger entry must be before a
	// positive liveness verdict re-stamps it. See the call site: this is
	// what keeps the stored ledger — and therefore the read-through cache
	// key — stable between polls.
	bgShellRefreshAfter = db.BgShellTTL / 4
	// monitorRefreshAfter is the same knob for the monitor ledger. Kept
	// separate from bgShellRefreshAfter so the two TTLs can diverge later
	// without silently coupling their write cadence.
	monitorRefreshAfter = db.MonitorTTL / 4
)

var bgShellReconcileMu struct {
	sync.Mutex
	last map[string]bgShellReadThrough
}

// Test-only callers replace this through SetBgShellDescendantCommandLinesForTest.
var bgShellDescendantCommandLines = session.DescendantCommandLines

// backgroundCounts is what one reconcile pass concluded about a session:
// how much of each kind of turn-outliving work is still believed to be
// running. Sub-agents are not here — they have their own hook pair and
// need no process evidence.
type backgroundCounts struct {
	Shells   int
	Monitors int
}

// any reports whether the session has any background work outstanding.
func (c backgroundCounts) any() bool { return c.Shells > 0 || c.Monitors > 0 }

// bgShellReadThrough is one session's cached reconcile verdict. The two
// ledger JSONs are the exact stored values the counts were derived from: a
// hook that adds or removes an entry in either changes one of them, which
// invalidates the cache immediately rather than making newly launched work
// wait out the interval.
type bgShellReadThrough struct {
	at          time.Time
	counts      backgroundCounts
	ledgerJSON  string
	monitorJSON string
}

// backgroundCountsOnRead returns how much turn-outliving work the agent
// behind this session row is believed to still be running, and persists
// any ledger corrections it derives on the way.
//
// Returns zeroes for anything that cannot have live background work: a row
// with no live tmux session (background shells are children of the harness
// process and monitors belong to it, so they died with it), a harness with
// neither concept (Codex), or empty ledgers.
//
// When the host's process table cannot be read the ledgers are left
// untouched and their TTL-filtered counts are returned — "cannot tell"
// must degrade to the hook's view, never to zero, or the badges would
// silently vanish on such a host.
func backgroundCountsOnRead(sess *db.SessionRow, alive bool) backgroundCounts {
	return backgroundCountsOnReadAt(sess, alive, time.Now())
}

// backgroundCountsOnReadAt is the clock-injected core used by periodic
// daemon reconciliation. Dashboard reads use backgroundCountsOnRead above;
// sharing the caller's sweep time keeps sub-agent TTL and background TTL
// decisions on the same observation boundary and makes the settle path
// deterministic in tests.
func backgroundCountsOnReadAt(sess *db.SessionRow, alive bool, now time.Time) backgroundCounts {
	var out backgroundCounts
	if sess == nil || !alive || sess.ID == "" {
		return out
	}
	// A ledger the harness has no concept of is not read at all, so a row
	// carrying a stale value from a harness switch cannot resurrect it.
	shellJSON, monitorJSON := "", ""
	if h, err := harness.Resolve(sess.Harness); err == nil {
		if h.SupportsBackgroundShells() {
			shellJSON = sess.BgShellsJSON
		}
		if h.SupportsMonitors() {
			monitorJSON = sess.MonitorsJSON
		}
	}
	if shellJSON == "" && monitorJSON == "" {
		return out
	}

	liveShells := db.ParseBgShellSet(shellJSON).Live(now)
	liveMonitors := db.ParseMonitorSet(monitorJSON).Live(now)
	if len(liveShells) == 0 && len(liveMonitors) == 0 {
		return out
	}
	if cached, ok := cachedBackgroundCounts(sess.ID, shellJSON, monitorJSON, now); ok {
		return cached
	}

	cmdlines, ok := bgShellDescendantCommandLines(sess.PID)
	if !ok {
		// No process-table evidence either way. Do NOT retire anything —
		// see DescendantCommandLines on why ok=false and "nothing running"
		// must not be conflated. Deliberately not cached: the next poll
		// should retry rather than pin this non-answer for an interval.
		return backgroundCounts{Shells: len(liveShells), Monitors: len(liveMonitors)}
	}

	verdict := session.ReconcileBackground(liveShells, liveMonitors, cmdlines)
	// Undecided entries (a command too short or too quoted to match on, or
	// a websocket monitor with no process at all) keep counting: the
	// reconcile has no opinion about them, so their deadline and the TTL
	// remain their only bounds.
	out.Shells = len(verdict.Shells.Alive) + len(verdict.Shells.Undecided)
	out.Monitors = len(verdict.Monitors.Alive) + len(verdict.Monitors.Undecided)

	persistReconciledShells(sess.ID, shellJSON, verdict.Shells, now)
	persistReconciledMonitors(sess.ID, monitorJSON, verdict.Monitors, now)
	storeBackgroundCounts(sess.ID, shellJSON, monitorJSON, out, now)
	return out
}

// persistReconciledShells writes back what the pass concluded about the
// background-shell ledger: retire what is gone, re-stamp what is provably
// still running.
//
// Entries are re-stamped only once they have gone stale, NOT on every
// poll. Re-stamping eagerly would rewrite the ledger on every dashboard
// tick, and since the cache is keyed on the stored value that would also
// mean a permanent cache miss — i.e. a full process-table walk plus a DB
// write per poll for as long as any background shell is running. Refreshing
// at a quarter of the TTL keeps entries ~4x clear of expiry while leaving
// the stored value stable in between.
func persistReconciledShells(sessionID, stored string, verdict session.BgShellLiveness, now time.Time) {
	if stored == "" {
		return
	}
	next := db.ParseBgShellSet(stored)
	for _, id := range verdict.Dead {
		next.Remove(id)
	}
	for _, id := range verdict.Alive {
		if e, known := next[id]; known && now.Sub(e.Seen) > bgShellRefreshAfter {
			next.Refresh(id, now)
		}
	}
	next.Sweep(now)
	if encoded := next.Encode(); encoded != stored {
		if _, err := db.SetSessionBgShellsIfUnchanged(sessionID, stored, encoded); err != nil {
			slog.Warn("bg-shells: failed to persist reconciled ledger",
				"session_id", sessionID, "error", err, "module", "agentd")
		}
	}
}

// persistReconciledMonitors is the monitor-ledger sibling of
// persistReconciledShells, on the same terms. Note Refresh moves only the
// last-seen stamp: a watch's harness-enforced deadline is an absolute bound
// taken at launch and is never extended by the watch still being alive now.
func persistReconciledMonitors(sessionID, stored string, verdict session.BgShellLiveness, now time.Time) {
	if stored == "" {
		return
	}
	next := db.ParseMonitorSet(stored)
	for _, id := range verdict.Dead {
		next.Remove(id)
	}
	for _, id := range verdict.Alive {
		if e, known := next[id]; known && now.Sub(e.Seen) > monitorRefreshAfter {
			next.Refresh(id, now)
		}
	}
	next.Sweep(now)
	if encoded := next.Encode(); encoded != stored {
		if _, err := db.SetSessionMonitorsIfUnchanged(sessionID, stored, encoded); err != nil {
			slog.Warn("monitors: failed to persist reconciled ledger",
				"session_id", sessionID, "error", err, "module", "agentd")
		}
	}
}

// cachedBackgroundCounts returns a session's cached verdict when it is both
// fresh and derived from the ledger values being read now.
func cachedBackgroundCounts(sessionID, ledgerJSON, monitorJSON string, now time.Time) (backgroundCounts, bool) {
	bgShellReconcileMu.Lock()
	defer bgShellReconcileMu.Unlock()
	cached, ok := bgShellReconcileMu.last[sessionID]
	if !ok || cached.ledgerJSON != ledgerJSON || cached.monitorJSON != monitorJSON ||
		now.Sub(cached.at) >= bgShellReconcileMinInterval {
		return backgroundCounts{}, false
	}
	return cached.counts, true
}

// storeBackgroundCounts records a fresh verdict, pruning entries no read has
// touched within bgShellCacheTTL so the cache tracks the live roster
// rather than every agent ever rendered.
func storeBackgroundCounts(sessionID, ledgerJSON, monitorJSON string, counts backgroundCounts, now time.Time) {
	bgShellReconcileMu.Lock()
	defer bgShellReconcileMu.Unlock()
	if bgShellReconcileMu.last == nil {
		bgShellReconcileMu.last = map[string]bgShellReadThrough{}
	}
	for id, e := range bgShellReconcileMu.last {
		if now.Sub(e.at) > bgShellCacheTTL {
			delete(bgShellReconcileMu.last, id)
		}
	}
	bgShellReconcileMu.last[sessionID] = bgShellReadThrough{
		at: now, counts: counts, ledgerJSON: ledgerJSON, monitorJSON: monitorJSON,
	}
}
