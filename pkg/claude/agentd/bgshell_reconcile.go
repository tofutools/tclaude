package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Background-shell liveness reconcile.
//
// Claude Code fires a PostToolUse hook when a `Bash` call is launched with
// run_in_background and NO hook at all when that command exits. The ledger
// the hook feeds (db.BgShellSet) can therefore only grow on its own, and
// the moment its count is read is exactly the idle window in which stale
// entries are most likely — so a hook-only badge would show ghosts
// precisely when it is looked at. That is why the previous decision was
// "no background-shell count" at all.
//
// This is the missing half: at dashboard read time, an agent's ledger is
// re-derived from the processes actually running below it. It runs on the
// read path rather than a polling loop for the same reason
// refreshCodexContextSnapshotOnRead does — the work is only needed when
// somebody is looking, and the dashboard poll already provides the cadence.

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
)

var bgShellReconcileMu struct {
	sync.Mutex
	last map[string]bgShellReadThrough
}

// bgShellReadThrough is one session's cached reconcile verdict. ledgerJSON
// is the exact stored ledger the count was derived from: a hook that adds
// or removes an entry changes it, which invalidates the cache immediately
// rather than making a newly launched shell wait out the interval.
type bgShellReadThrough struct {
	at         time.Time
	count      int
	ledgerJSON string
}

// bgShellCountOnRead returns how many background shell commands the agent
// behind this session row is believed to still be running, and persists
// any ledger corrections it derives on the way.
//
// Returns 0 for anything that cannot have a live background shell: a row
// with no live tmux session (background shells are children of the harness
// process, so they died with it), a harness with no background-shell
// concept (Codex), or an empty ledger.
//
// When the host's process table cannot be read the ledger is left
// untouched and its TTL-filtered count is returned — "cannot tell" must
// degrade to the hook's view, never to zero, or the badge would silently
// vanish on such a host.
func bgShellCountOnRead(sess *db.SessionRow, alive bool) int {
	if sess == nil || !alive || sess.ID == "" || sess.BgShellsJSON == "" {
		return 0
	}
	if h, err := harness.Resolve(sess.Harness); err != nil || !h.SupportsBackgroundShells() {
		return 0
	}
	now := time.Now()
	stored := db.ParseBgShellSet(sess.BgShellsJSON)
	live := stored.Live(now)
	if len(live) == 0 {
		return 0
	}
	if cached, ok := cachedBgShellCount(sess.ID, sess.BgShellsJSON, now); ok {
		return cached
	}

	cmdlines, ok := session.DescendantCommandLines(sess.PID)
	if !ok {
		// No process-table evidence either way. Do NOT retire anything —
		// see DescendantCommandLines on why ok=false and "nothing running"
		// must not be conflated. Deliberately not cached: the next poll
		// should retry rather than pin this non-answer for an interval.
		return len(live)
	}

	verdict := session.ReconcileBgShells(live, cmdlines)
	// Undecided entries (a command too short or too quoted to match on)
	// keep counting: the reconcile has no opinion about them, so the TTL
	// remains their only bound.
	count := len(verdict.Alive) + len(verdict.Undecided)

	next := db.ParseBgShellSet(sess.BgShellsJSON)
	for _, id := range verdict.Dead {
		next.Remove(id)
	}
	for _, id := range verdict.Alive {
		// Re-stamp what is provably still running, so a background shell
		// that legitimately runs for hours is never aged out by the TTL
		// backstop on a host where this reconcile works.
		//
		// Only once an entry has gone stale, NOT on every poll. Re-stamping
		// eagerly would rewrite the ledger on every dashboard tick, and
		// since the cache is keyed on the stored value that would also mean
		// a permanent cache miss — i.e. a full process-table walk plus a DB
		// write per poll for as long as any background shell is running.
		// Refreshing at a quarter of the TTL keeps entries ~4x clear of
		// expiry while leaving the stored value stable in between.
		if e, known := next[id]; known && now.Sub(e.Seen) > bgShellRefreshAfter {
			next.Refresh(id, now)
		}
	}
	next.Sweep(now)
	if encoded := next.Encode(); encoded != sess.BgShellsJSON {
		if _, err := db.SetSessionBgShellsIfUnchanged(sess.ID, sess.BgShellsJSON, encoded); err != nil {
			slog.Warn("bg-shells: failed to persist reconciled ledger",
				"session_id", sess.ID, "error", err, "module", "agentd")
		}
	}
	storeBgShellCount(sess.ID, sess.BgShellsJSON, count, now)
	return count
}

// cachedBgShellCount returns a session's cached verdict when it is both
// fresh and derived from the ledger value being read now.
func cachedBgShellCount(sessionID, ledgerJSON string, now time.Time) (int, bool) {
	bgShellReconcileMu.Lock()
	defer bgShellReconcileMu.Unlock()
	cached, ok := bgShellReconcileMu.last[sessionID]
	if !ok || cached.ledgerJSON != ledgerJSON || now.Sub(cached.at) >= bgShellReconcileMinInterval {
		return 0, false
	}
	return cached.count, true
}

// storeBgShellCount records a fresh verdict, pruning entries no read has
// touched within bgShellCacheTTL so the cache tracks the live roster
// rather than every agent ever rendered.
func storeBgShellCount(sessionID, ledgerJSON string, count int, now time.Time) {
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
	bgShellReconcileMu.last[sessionID] = bgShellReadThrough{at: now, count: count, ledgerJSON: ledgerJSON}
}
