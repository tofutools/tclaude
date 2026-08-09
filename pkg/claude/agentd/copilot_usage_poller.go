package agentd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The daemon-owned sweep of Copilot's own SQLite session store.
//
// Copilot is the one supported harness whose durable event log cannot produce a
// live token meter — copilot_telemetry.go explains why at length: the three
// events that carry per-call usage are all flagged `ephemeral` and never reach
// the disk. The CLI does persist that accounting, in a SQLite store next to the
// session-state tree, and this file is the read-only poll of it.
//
// It is ONE background sweep for the whole daemon, not a poller per
// conversation, and that shape is deliberate. Every live Copilot pane under one
// COPILOT_HOME reads from the same database file: giving each its own
// connection and its own query would multiply the file handles, the WAL reader
// slots and the busy contention against a database owned by a process tclaude
// does not control, to fetch rows one statement could have returned together.
// So the sweep resolves each session's home, buckets by it, and issues one
// batched range-scan per home per tick.
//
// What it does NOT do is render anything. The rows land in
// copilot_usage_snapshots (the accounting) and in the shared session context
// columns (the display), and the dashboard's existing ~2s poll draws them with
// no new frontend channel — the same columns it already draws for every other
// harness.

const (
	// copilotUsagePollInterval matches copilotContextRefreshInterval and the
	// dashboard's own poll, so a live pane's numbers are at most one tick
	// behind the fetch that will render them. Polling faster would buy latency
	// nobody can see, against someone else's database.
	copilotUsagePollInterval = 2 * time.Second

	// copilotUsageSweepRowLimit bounds one sweep's total rows.
	//
	// The cap exists for first sight of a long-running session, where the
	// backlog is every call it has ever made. Draining that over a few ticks
	// keeps one query from stalling behind an unbounded read; the checkpoint
	// makes it resumable, so the only cost of hitting the cap is that the
	// meter catches up a few seconds later.
	copilotUsageSweepRowLimit = 2000

	// copilotUsageHomeBackoff is how long a home stays untouched after it fails
	// to open or fails its schema probe.
	//
	// A missing table or a renamed column is a property of the installed
	// Copilot release, not of this tick: retrying it every 2 seconds would
	// re-probe a database that cannot have changed, forever. 60s means an
	// operator who upgrades or repairs Copilot sees the meter return within a
	// minute without the daemon having spun in between.
	copilotUsageHomeBackoff = time.Minute

	// copilotUsageWarnInterval suppresses repeat log lines per home and reason.
	// At a 2s cadence an unsuppressed line is 1800 entries an hour saying the
	// same unchanging thing.
	copilotUsageWarnInterval = 5 * time.Minute
)

// copilotUsageLiveEntry is what the sweep publishes for the read-through
// follower to merge. See copilot_context_refresh.go for the precedence rule
// this exists to implement.
type copilotUsageLiveEntry struct {
	ConvID    string
	CreatedAt time.Time

	// ContextTokens is the newest call's full prompt — the live occupancy
	// NUMERATOR. It is one call's figure, never a sum: the conversation prefix
	// is re-sent every turn, so summing input_tokens across calls would produce
	// an occupancy several times the size of the window.
	ContextTokens int64
	// OutputTokens is cumulative across every call the sweep has consumed.
	OutputTokens int64
	// Model is the newest top-level call's model id. The read-through follower
	// needs it to resolve the same effective denominator the sweep uses
	// (configured cap, else this model's static assumption) — without it the
	// follower could only measure against the observed window column, which
	// Copilot rarely discloses, and the two writers would flap the row between
	// a real percentage and 0.
	Model string

	// LastEventID is the cursor, kept here so a sweep that is already up to
	// date issues no query work for this session beyond its range scan.
	LastEventID int64
	// loaded records that the durable cursor has been read once this daemon
	// lifetime, so a session with genuinely no rows yet is not re-read from
	// tclaude's DB on every tick.
	loaded bool
}

// copilotUsageState is the sweep's whole memory. Both maps are keyed by tclaude
// session id and are pruned to the live set on every tick, so a finished pane
// cannot hold a store handle or a cursor open indefinitely.
var copilotUsageState struct {
	sync.Mutex
	sessions map[string]*copilotUsageLiveEntry
	homes    map[string]*copilotUsageHome
	stopping bool
	// liveCount is the last matched-session count that was logged, so the
	// observability line below reports CHANGES rather than repeating the same
	// number every 2 seconds. liveCountKnown keeps "nothing logged yet" apart
	// from "logged a zero".
	liveCount      int
	liveHomes      int
	liveCountKnown bool
}

// copilotUsageHome is one COPILOT_HOME's cached read-only connection plus its
// failure bookkeeping.
type copilotUsageHome struct {
	store *harness.CopilotUsageStore
	// downUntil suppresses reopen attempts after a failure.
	downUntil time.Time
	// warnedAt is keyed by reason so a NEW failure mode is reported promptly
	// even while an old one is being suppressed.
	warnedAt map[string]time.Time
}

// startCopilotUsagePoller runs the sweep until stop closes. Mirrors
// startCodexUsagePoller's shape, including that it never returns an error to
// its caller: a harness whose telemetry is unavailable degrades the meter, it
// does not degrade the daemon.
func startCopilotUsagePoller(stop <-chan struct{}) {
	// One Info line, once, at startup. The sweep is otherwise entirely silent
	// in the healthy case, which left an operator debugging a blank meter with
	// no way to tell "the poller is running and finding nothing" apart from
	// "the poller never started". This is the cheapest possible answer to that
	// question and it costs one line per daemon lifetime.
	slog.Info("copilot-usage: poller started",
		"interval", copilotUsagePollInterval, "module", "agentd")
	go func() {
		ticker := time.NewTicker(copilotUsagePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				stopCopilotUsagePoller()
				return
			case <-ticker.C:
				sweepCopilotUsage(context.Background())
			}
		}
	}()
}

// stopCopilotUsagePoller closes every cached store and refuses further sweeps.
func stopCopilotUsagePoller() {
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	copilotUsageState.stopping = true
	for _, home := range copilotUsageState.homes {
		if home.store != nil {
			_ = home.store.Close()
		}
	}
	copilotUsageState.homes = nil
	copilotUsageState.sessions = nil
	copilotUsageState.liveCountKnown = false
}

// sweepCopilotUsage is one tick: find the live Copilot panes, read everything
// new for them, and persist it.
//
// The early return on "no live Copilot sessions" is the steady state on a host
// that does not run Copilot, and it costs no file I/O at all — nothing is
// opened, and any store cached by an earlier sweep is closed.
func sweepCopilotUsage(ctx context.Context) {
	sessions, ok := liveCopilotSessions()
	if !ok {
		// Liveness or the session list was UNAVAILABLE, which is not the same
		// as "no Copilot panes are running". Pruning on it would close every
		// cached store and drop every cursor over a transient tmux hiccup,
		// making the next successful sweep re-open and re-probe each home for
		// nothing. Skip the tick instead and keep what we have.
		return
	}
	if len(sessions) == 0 {
		noteCopilotUsageLiveCount(0, 0)
		pruneCopilotUsageState(nil)
		return
	}
	byHome := map[string][]*db.SessionRow{}
	for _, sess := range sessions {
		home := copilotHomeForSession(sess)
		if home == "" {
			// No resolvable COPILOT_HOME means no store to read. Not an error:
			// the same condition the conversation store reports as "no
			// conversations", and it must not warn on a 2s loop.
			continue
		}
		byHome[home] = append(byHome[home], sess)
	}
	// Reported AFTER the home filter as well as before it, because the two
	// numbers fail differently: sessions=0 is a liveness/harness/conv-id
	// question, while sessions=3 homes=0 says every live pane resolved to no
	// COPILOT_HOME at all — a distinct diagnosis that a single count hides.
	noteCopilotUsageLiveCount(len(sessions), len(byHome))
	pruneCopilotUsageState(sessions)
	for home, rows := range byHome {
		sweepCopilotUsageHome(ctx, home, rows)
	}
}

// noteCopilotUsageLiveCount reports how many live Copilot panes matched the
// sweep's filter, and how many distinct COPILOT_HOMEs they resolved to, on
// CHANGE rather than per tick.
//
// Both numbers are real diagnostics. A session is dropped from the first for a
// dead pane, a non-Copilot harness, a missing tmux session or conv id, or a
// conv id that cannot be a Copilot session uuid; it is dropped from the second
// when its launch environment names no usable COPILOT_HOME. When the meter is
// blank, "matched 0 sessions", "3 sessions, 0 homes" and "3 sessions, 1 home"
// send an operator to three completely different places, and none of them was
// previously observable at any log level.
func noteCopilotUsageLiveCount(sessions, homes int) {
	copilotUsageState.Lock()
	quiet := copilotUsageState.liveCountKnown &&
		copilotUsageState.liveCount == sessions && copilotUsageState.liveHomes == homes
	copilotUsageState.liveCount = sessions
	copilotUsageState.liveHomes = homes
	copilotUsageState.liveCountKnown = true
	copilotUsageState.Unlock()
	if quiet {
		return
	}
	slog.Debug("copilot-usage: live Copilot sessions matched",
		"sessions", sessions, "homes", homes, "module", "agentd")
}

// liveCopilotSessions returns the Copilot session rows whose tmux pane is
// actually alive. A dead pane's store rows cannot grow, and its row already
// holds the final reading.
//
// The bool reports whether the answer is TRUSTWORTHY, which the caller needs
// kept apart from the empty slice: "no Copilot panes are running" is a real
// observation that should retire cached state, while "tmux did not answer" is
// the absence of an observation and must change nothing.
func liveCopilotSessions() ([]*db.SessionRow, bool) {
	// Through the daemon-wide coalescing cache, not a direct probe: this sweep
	// ticks at the same 2s cadence as the dashboard's poll handlers, so it
	// almost always rides their `tmux ls` instead of forking its own.
	alive, err := cachedLiveTmuxSessions()
	if err != nil {
		slog.Debug("copilot-usage: tmux liveness unavailable; skipping sweep",
			"error", err, "module", "agentd")
		return nil, false
	}
	rows, err := db.ListSessions()
	if err != nil {
		slog.Debug("copilot-usage: session list unavailable; skipping sweep",
			"error", err, "module", "agentd")
		return nil, false
	}
	var live []*db.SessionRow
	for _, row := range rows {
		if row == nil || row.Harness != harness.CopilotName ||
			row.ID == "" || row.ConvID == "" || row.TmuxSession == "" {
			continue
		}
		if _, ok := alive[row.TmuxSession]; !ok {
			continue
		}
		// A conv id carrying a separator or a `..` is not a Copilot session id.
		// It is used as a SQL parameter rather than a path here, so it cannot
		// escape a directory — but it also cannot be a real row's session_id,
		// so querying for it is pure waste.
		if !harness.CopilotSafeConvID(row.ConvID) {
			continue
		}
		live = append(live, row)
	}
	return live, true
}

// copilotHomeForSession resolves the COPILOT_HOME this session's pane actually
// uses.
//
// A sandbox profile may relocate COPILOT_HOME — unlike HOME, it is not a
// reserved variable — so a spawned agent's store can sit somewhere other than
// the operator's ~/.copilot. Reading the ambient home for such a pane would
// silently attribute a different (or empty) store to it, so the session's own
// frozen launch environment wins whenever it names one.
func copilotHomeForSession(sess *db.SessionRow) string {
	if sess != nil && sess.EffectiveSandbox != nil {
		for _, entry := range sess.EffectiveSandbox.Effective.Environment {
			if entry.Name != harness.CopilotHomeEnvVar {
				continue
			}
			value := strings.TrimSpace(entry.Value)
			// Only an absolute path is usable: a relative COPILOT_HOME would
			// resolve against the daemon's cwd, which is not the pane's.
			if filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
			break
		}
	}
	return harness.CopilotHome()
}

// pruneCopilotUsageState drops per-session memory for panes that are no longer
// live, and closes any home no live pane still resolves to.
func pruneCopilotUsageState(live []*db.SessionRow) {
	keep := make(map[string]struct{}, len(live))
	keepHomes := make(map[string]struct{}, len(live))
	for _, sess := range live {
		keep[sess.ID] = struct{}{}
		if home := copilotHomeForSession(sess); home != "" {
			keepHomes[home] = struct{}{}
		}
	}
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	for id := range copilotUsageState.sessions {
		if _, ok := keep[id]; !ok {
			delete(copilotUsageState.sessions, id)
		}
	}
	for path, home := range copilotUsageState.homes {
		if _, ok := keepHomes[path]; ok {
			continue
		}
		if home.store != nil {
			_ = home.store.Close()
		}
		delete(copilotUsageState.homes, path)
	}
}

// sweepCopilotUsageHome reads one home's store for every session under it.
func sweepCopilotUsageHome(ctx context.Context, home string, sessions []*db.SessionRow) {
	cursors := make([]harness.CopilotUsageCursor, 0, len(sessions))
	byConv := make(map[string][]*db.SessionRow, len(sessions))
	for _, sess := range sessions {
		// Loaded BEFORE the store is opened, deliberately: the durable cursor
		// is where the restart backfill comes from, and a session whose
		// model/effort are already known must be restored to the dashboard even
		// on a host where the store has since become unreadable.
		entry, restored := copilotUsageEntryFor(sess)
		if restored != nil {
			backfillCopilotUsageContext(sess, *restored)
		}
		cursors = append(cursors, harness.CopilotUsageCursor{
			SessionID: sess.ConvID, AfterEventID: entry.LastEventID,
		})
		// Two tclaude sessions CAN share a conv id — a resumed conversation
		// keeps its Copilot session uuid — so a conv id maps to a list, and
		// every row is folded into each of them rather than into whichever one
		// happened to be found first.
		byConv[sess.ConvID] = append(byConv[sess.ConvID], sess)
	}
	store, ok := copilotUsageStoreFor(home)
	if !ok {
		return
	}
	calls, err := store.Calls(ctx, cursors, copilotUsageSweepRowLimit)
	if err != nil {
		// SQLITE_BUSY lands here too, and is still NOT a reason to close the
		// store or back the home off: Copilot's writer holding the database for
		// a moment is the normal case, and the next tick is 2 seconds away. The
		// cursor means nothing was lost by skipping this one.
		//
		// It IS a reason to log at Error, rate-limited. This used to be Debug on
		// the theory that the failure is transient — and then a real,
		// permanent failure (Copilot writing REAL into an INTEGER latency
		// column, which the reader scanned as int64) failed this read on every
		// tick of every real host and said so nowhere an operator would look.
		// The rate limiter is what makes the two cases sit together: a genuinely
		// transient busy-timeout logs once and is then quiet, and a permanent
		// one logs once every copilotUsageWarnInterval instead of hiding.
		logCopilotUsageHomeFailure(home, "read",
			"copilot-usage: sweep read failed; live usage is not being recorded", err)
		return
	}
	if len(calls) == 0 {
		return
	}
	for convID, rows := range byConv {
		convCalls := callsForConv(calls, convID)
		if len(convCalls) == 0 {
			continue
		}
		for _, sess := range rows {
			applyCopilotUsageCalls(sess, convCalls)
		}
	}
}

// callsForConv slices out one conversation's rows. The query returns them
// ordered by (session_id, id), so this is a contiguous run and the per-session
// fold below can rely on ascending event ids.
func callsForConv(calls []harness.CopilotUsageCall, convID string) []harness.CopilotUsageCall {
	var out []harness.CopilotUsageCall
	for _, call := range calls {
		if call.SessionID == convID {
			out = append(out, call)
		}
	}
	return out
}

// copilotUsageStoreFor returns the cached read-only handle for a home, opening
// it if needed and honouring the failure backoff.
//
// Every failure path here degrades: the durable-log follower in
// copilot_context_refresh.go keeps running untouched, so the session still gets
// the context readings Copilot's event log discloses at a compaction,
// truncation or shutdown. The meter loses resolution; nothing breaks.
func copilotUsageStoreFor(home string) (*harness.CopilotUsageStore, bool) {
	copilotUsageState.Lock()
	if copilotUsageState.stopping {
		copilotUsageState.Unlock()
		return nil, false
	}
	if copilotUsageState.homes == nil {
		copilotUsageState.homes = map[string]*copilotUsageHome{}
	}
	entry := copilotUsageState.homes[home]
	if entry == nil {
		entry = &copilotUsageHome{warnedAt: map[string]time.Time{}}
		copilotUsageState.homes[home] = entry
	}
	if entry.store != nil {
		store := entry.store
		copilotUsageState.Unlock()
		return store, true
	}
	if time.Now().Before(entry.downUntil) {
		copilotUsageState.Unlock()
		return nil, false
	}
	copilotUsageState.Unlock()

	// Opened OUTSIDE the lock: a schema probe touches the filesystem, and
	// holding the sweep's only mutex across that would stall every other home.
	store, err := harness.OpenCopilotUsageStore(home)
	if err != nil {
		markCopilotUsageHomeDown(home, err)
		return nil, false
	}

	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	if copilotUsageState.stopping || copilotUsageState.homes == nil {
		_ = store.Close()
		return nil, false
	}
	entry = copilotUsageState.homes[home]
	if entry == nil {
		entry = &copilotUsageHome{warnedAt: map[string]time.Time{}}
		copilotUsageState.homes[home] = entry
	}
	if entry.store != nil {
		// Another goroutine won the race. Close ours rather than leak it.
		_ = store.Close()
		return entry.store, true
	}
	entry.store = store
	entry.downUntil = time.Time{}
	slog.Debug("copilot-usage: opened session store read-only",
		"path", store.Path(), "schema_version", store.SchemaVersion(), "module", "agentd")
	return store, true
}

// markCopilotUsageHomeDown backs a home off after an open or probe failure.
//
// The log level follows the operator's rule, and the distinguishing test is
// simply whether the database FILE IS THERE:
//
//   - Absent — Copilot is not installed, or has never run under this home. The
//     ordinary state of most hosts, so it must never produce an error line; it
//     backs off with a rate-limited Debug naming the path it looked for, which
//     is what an operator needs when the meter is blank and they want to know
//     WHICH path was consulted.
//   - Present but unusable — schema drift, a permissions problem, the
//     WAL-without-shm case where a read-only open of an idle database
//     legitimately fails. tclaude is supposed to be reading this file and
//     cannot: that is operator-visible by definition, so it logs at ERROR,
//     rate-limited per (home, reason) so a permanent condition reports once
//     rather than every 2 seconds.
func markCopilotUsageHomeDown(home string, err error) {
	if isCopilotUsageStoreAbsent(err) {
		if copilotUsageLogAllowed(home, "absent") {
			slog.Debug("copilot-usage: no Copilot session store for this home; live usage unavailable",
				"home", home, "path", harness.CopilotUsageStorePath(home), "module", "agentd")
		}
		setCopilotUsageHomeBackoff(home)
		return
	}
	logCopilotUsageHomeFailure(home, "open",
		"copilot-usage: session store unusable; live usage degrades to the durable event log", err)
	setCopilotUsageHomeBackoff(home)
}

func setCopilotUsageHomeBackoff(home string) {
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	if copilotUsageState.homes == nil {
		return
	}
	entry := copilotUsageState.homes[home]
	if entry == nil {
		entry = &copilotUsageHome{warnedAt: map[string]time.Time{}}
		copilotUsageState.homes[home] = entry
	}
	entry.downUntil = time.Now().Add(copilotUsageHomeBackoff)
}

// logCopilotUsageHomeFailure reports a home whose store EXISTS but could not be
// read, at most once per (home, reason) per copilotUsageWarnInterval.
//
// Error, not Debug. The previous level is the reason this bug lived: the sweep
// read failed on every single tick of every real host for as long as the
// feature had shipped, and said so only at Debug, so the only visible symptom
// was blank dashboard cells with no explanation anywhere an operator looks. A
// file tclaude is supposed to be reading and cannot is an error; keying the
// suppression by reason keeps a NEW failure mode audible while an old one is
// being held down.
func logCopilotUsageHomeFailure(home, reason, message string, err error) {
	if !copilotUsageLogAllowed(home, reason) {
		return
	}
	slog.Error(message, "home", home, "reason", reason,
		"path", harness.CopilotUsageStorePath(home), "error", err, "module", "agentd")
}

// copilotUsageLogAllowed is the shared per-(home, reason) rate limiter. It
// reports whether the caller may log now, and records the decision.
func copilotUsageLogAllowed(home, reason string) bool {
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	if copilotUsageState.homes == nil {
		// Created rather than skipped: without somewhere to record the decision
		// the limiter forgets it immediately and a permanent condition logs on
		// every 2s tick, which is the failure mode this whole function exists
		// to prevent.
		copilotUsageState.homes = map[string]*copilotUsageHome{}
	}
	entry := copilotUsageState.homes[home]
	if entry == nil {
		entry = &copilotUsageHome{warnedAt: map[string]time.Time{}}
		copilotUsageState.homes[home] = entry
	}
	if entry.warnedAt == nil {
		entry.warnedAt = map[string]time.Time{}
	}
	now := time.Now()
	if last, seen := entry.warnedAt[reason]; seen && now.Sub(last) < copilotUsageWarnInterval {
		return false
	}
	entry.warnedAt[reason] = now
	return true
}

// copilotUsageEntryFor returns the session's live state, loading its durable
// cursor once per daemon lifetime.
//
// The second return is the STORED snapshot, non-nil only on the load that
// actually read it — the caller's cue to run the restart backfill. It is
// returned rather than acted on here so that this function keeps its single
// job, and so "once per session per daemon lifetime" is a consequence of the
// cache rather than a second flag to keep in step with it.
func copilotUsageEntryFor(sess *db.SessionRow) (copilotUsageLiveEntry, *db.CopilotUsageSnapshot) {
	copilotUsageState.Lock()
	if copilotUsageState.sessions == nil {
		copilotUsageState.sessions = map[string]*copilotUsageLiveEntry{}
	}
	entry := copilotUsageState.sessions[sess.ID]
	if entry != nil && entry.ConvID == sess.ConvID &&
		entry.CreatedAt.Equal(sess.CreatedAt) && entry.loaded {
		snapshot := *entry
		copilotUsageState.Unlock()
		return snapshot, nil
	}
	copilotUsageState.Unlock()

	// Loaded OUTSIDE the lock for the same reason the store is opened outside
	// it. A duplicate load is harmless — both read the same committed row.
	fresh := &copilotUsageLiveEntry{
		ConvID: sess.ConvID, CreatedAt: sess.CreatedAt, loaded: true,
	}
	var restored *db.CopilotUsageSnapshot
	stored, err := db.LoadCopilotUsageSnapshot(sess.ID)
	if err != nil {
		slog.Warn("copilot-usage: failed to load snapshot cursor; restarting from the beginning",
			"session_id", sess.ID, "error", err, "module", "agentd")
	} else if stored != nil && stored.ConvID == sess.ConvID &&
		stored.FoldVersion == db.CopilotUsageFoldVersion {
		// A stored cursor whose conv id does NOT match belongs to a previous
		// generation of this session id. Ignoring it restarts the fold, which
		// is right: the new conversation's rows start at their own ids and the
		// old cursor would skip them. The fold version has the same fail-closed
		// rule: an old writer may know this session but not the current per-call
		// accounting semantics, so its cursor must never hide the prefix.
		fresh.LastEventID = stored.LastEventID
		fresh.ContextTokens = stored.LastCallInputTokens
		fresh.OutputTokens = stored.OutputTokens
		fresh.Model = stored.Model
		restored = stored
	}

	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	if copilotUsageState.sessions == nil {
		copilotUsageState.sessions = map[string]*copilotUsageLiveEntry{}
	}
	copilotUsageState.sessions[sess.ID] = fresh
	return *fresh, restored
}

// backfillCopilotUsageContext republishes an ALREADY-KNOWN reading to the
// dashboard columns, the first time a daemon sees a live session that has one.
//
// Without it, a daemon restart leaves an idle Copilot pane blank until it takes
// another turn: sessions.model and sessions.effort_level are owned by this
// sweep once a usage row exists, and the sweep only wrote them when new rows
// arrived. A conversation that is mid-thought — the common state of a pane an
// operator is reading — produces no new rows at all, so the columns stayed
// empty while the answer sat in copilot_usage_snapshots the whole time.
//
// It writes through persistCopilotUsageContext rather than around it, so the
// restored values are subject to exactly the same discipline as a live fold:
// the generation guard, the one-owner rule, and max() on tokens_output. And
// because that function skips the write when the row already agrees, a session
// whose columns are already correct costs one read and nothing else.
func backfillCopilotUsageContext(sess *db.SessionRow, stored db.CopilotUsageSnapshot) {
	if stored.LastEventID <= 0 {
		// Nothing has ever been folded for this session, so there is nothing to
		// restore — only the empty row a first sweep would have created.
		return
	}
	slog.Debug("copilot-usage: restoring stored usage for a live session",
		"session_id", sess.ID, "model", stored.Model,
		"effort", stored.ReasoningEffort, "last_event_id", stored.LastEventID,
		"module", "agentd")
	persistCopilotUsageContext(sess, stored)
}

// applyCopilotUsageCalls folds one session's new rows and persists the result.
//
// The fold is additive for the cumulative columns and last-wins for the
// per-call ones, which is why the caller must hand it rows in ascending event
// id: the LAST row in the slice is the newest call and decides the occupancy
// numerator, the model, the effort and the finish reason.
func applyCopilotUsageCalls(sess *db.SessionRow, calls []harness.CopilotUsageCall) {
	if len(calls) == 0 {
		return
	}
	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	if err != nil {
		slog.Warn("copilot-usage: failed to read snapshot before update",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	next := db.CopilotUsageSnapshot{SessionID: sess.ID, ConvID: sess.ConvID}
	var priorTotalNanoAIU *int64
	if snapshot != nil && snapshot.ConvID == sess.ConvID &&
		snapshot.FoldVersion == db.CopilotUsageFoldVersion {
		next = *snapshot
		next.SessionID = sess.ID
		next.ConvID = sess.ConvID
		priorTotalNanoAIU = snapshot.TotalNanoAIU
	}
	var nanoAIU int64
	var sawNanoAIU bool
	if next.TotalNanoAIU != nil {
		nanoAIU = *next.TotalNanoAIU
		sawNanoAIU = true
	}
	for _, call := range calls {
		if call.EventID <= next.LastEventID {
			// Defence in depth against a re-delivered row: the query already
			// excludes them, but the cumulative columns are the one place a
			// duplicate would corrupt rather than merely repeat.
			continue
		}
		// EVERY row advances the cursor and the totals, nested or not: a call
		// made from inside a tool call is real spend and must be accounted for,
		// and skipping its id would re-read it forever.
		next.LastEventID = call.EventID
		next.Requests++
		next.InputTokens += call.InputTokens
		next.OutputTokens += call.OutputTokens
		next.CacheReadTokens += call.CacheReadTokens
		next.CacheWriteTokens += call.CacheWriteTokens
		next.ReasoningTokens += call.ReasoningTokens
		// total_nano_aiu is the cost of THIS CALL. Both the pinned 1.0.77 and
		// measured 1.0.78 schemas describe the matching assistant.usage value
		// as "for this request"; 1.0.78 stores confirm that summing these rows
		// exactly reaches the durable session checkpoint. Nested calls are real
		// billed calls too, so they participate in the sum.
		if call.HasNanoAIU {
			nanoAIU += call.TotalNanoAIU
			sawNanoAIU = true
		}

		// Only a TOP-LEVEL call describes the main conversation, so only it may
		// move the occupancy numerator and the fields rendered beside it.
		//
		// Copilot records a call made from inside a tool call under the same
		// session_id (the row carries parent_tool_call_id for exactly that
		// reason). Such a call has its own, typically much smaller, prompt —
		// so last-row-wins would take it as the conversation's context and dip
		// the meter every time a tool ran, then restore it on the next real
		// turn. The totals above still count it; it just does not get to say
		// how full the conversation's window is.
		if call.Nested {
			continue
		}
		next.LastTurnIndex = call.TurnIndex
		if call.Model != "" {
			next.Model = call.Model
		}
		next.ReasoningEffort = call.ReasoningEffort
		next.FinishReason = call.FinishReason
		if call.RequestMultiplier > 0 {
			multiplier := call.RequestMultiplier
			next.RequestMultiplier = &multiplier
		}
		next.LastCallInputTokens = call.ContextTokens()
		next.LastCallOutputTokens = call.OutputTokens
		next.LastCallCacheReadTokens = call.CacheReadTokens
		next.LastCallCacheWriteTokens = call.CacheWriteTokens
		next.LastDurationMs = call.DurationMs
		next.LastTimeToFirstTokenMs = call.TimeToFirstTokenMs
		next.LastInterTokenLatencyMs = call.InterTokenLatencyMs
		next.LastCallStamp = call.CreatedAt
	}
	if sawNanoAIU {
		next.TotalNanoAIU = &nanoAIU
	}
	next.ObservedAt = time.Now().UTC()

	saved, err := db.SaveCopilotUsageSnapshot(next, sess.CreatedAt)
	if err != nil {
		slog.Warn("copilot-usage: failed to persist snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	if !saved {
		// The generation moved on between the read and the write. Drop the
		// in-memory entry so the next sweep reloads for the new generation
		// rather than continuing a fold that belongs to a dead conversation.
		copilotUsageState.Lock()
		delete(copilotUsageState.sessions, sess.ID)
		copilotUsageState.Unlock()
		return
	}
	// The Copilot store's native counter is a gross subscription-value
	// estimate, never actual spend. Keep the write generation-guarded just like
	// the snapshot: an old fold must not put its virtual dollars on a reused
	// session id. Nil/zero nano-AIU deliberately leaves virtual_cost_usd
	// untouched at zero, so an unmeasured call never renders as $0.00.
	if virtual, ok := harness.CopilotVirtualCostFromNanoAIU(next.TotalNanoAIU); ok &&
		(priorTotalNanoAIU == nil || *priorTotalNanoAIU != *next.TotalNanoAIU) {
		persisted, err := db.UpdateSessionVirtualCostForGeneration(
			sess.ID, sess.ConvID, sess.CreatedAt, virtual.USD)
		if err != nil {
			slog.Warn("copilot-usage: failed to persist virtual cost",
				"session_id", sess.ID, "error", err, "module", "agentd")
		} else if !persisted {
			copilotUsageState.Lock()
			delete(copilotUsageState.sessions, sess.ID)
			copilotUsageState.Unlock()
			return
		}
	}
	publishCopilotUsage(sess, next)
	persistCopilotUsageContext(sess, next)
}

// publishCopilotUsage updates the in-memory view the read-through follower
// merges. See copilot_context_refresh.go for why the two must agree.
func publishCopilotUsage(sess *db.SessionRow, snapshot db.CopilotUsageSnapshot) {
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	if copilotUsageState.sessions == nil {
		copilotUsageState.sessions = map[string]*copilotUsageLiveEntry{}
	}
	copilotUsageState.sessions[sess.ID] = &copilotUsageLiveEntry{
		ConvID:        sess.ConvID,
		CreatedAt:     sess.CreatedAt,
		ContextTokens: snapshot.LastCallInputTokens,
		OutputTokens:  snapshot.OutputTokens,
		Model:         snapshot.Model,
		LastEventID:   snapshot.LastEventID,
		loaded:        true,
	}
}

// lookupCopilotLiveUsage returns the sweep's current figures for a session
// generation, or false when the sweep has nothing for it.
//
// False is the state that keeps the durable follower fully in charge: a
// session whose store has never produced a row, a host where the store is
// unreadable, and a daemon whose poller has not run yet all land here.
func lookupCopilotLiveUsage(sessionID, convID string, createdAt time.Time) (copilotUsageLiveEntry, bool) {
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	entry := copilotUsageState.sessions[sessionID]
	if entry == nil || entry.ConvID != convID || !entry.CreatedAt.Equal(createdAt) ||
		entry.LastEventID <= 0 {
		return copilotUsageLiveEntry{}, false
	}
	return *entry, true
}

// persistCopilotUsageContext writes the shared context columns the dashboard
// already renders, plus the observed model and effort. The durable Copilot
// context follower also reads model-change events, but deliberately does not
// write sessions.model or sessions.effort_level; once a usage row exists, this
// sweep is the sole owner of those two dashboard columns, so they cannot flap.
//
// The denominator is the shared effective resolution: an explicit Copilot cap
// persisted with the conversation's relaunch profile, else the static
// assumption for the observed model, else whatever window the durable
// follower has recorded as disclosed. Copilot does not report a context cap
// on its own, so this must not overwrite context_window_size, which remains
// the observed snapshot owned by the durable context follower. With no cap
// resolvable from any arm, the percentage stays 0 while token counts are
// still written.
func persistCopilotUsageContext(sess *db.SessionRow, snapshot db.CopilotUsageSnapshot) {
	stored, err := db.GetContextSnapshot(sess.ID)
	if err != nil {
		slog.Warn("copilot-usage: failed to read context window; skipping context write",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	// STAND DOWN on the context columns when the API state consumer has a live
	// reading for this conversation. It reads the occupancy and the window from
	// Copilot itself, where this sweep infers the first from the newest call's
	// prompt and the second from a static per-model table.
	//
	// sessions.model and sessions.effort_level are still this sweep's to own on
	// both drives — the API reading has no source for effort at all — so the
	// write becomes the narrow one that names only those two columns. Passing
	// the stored context values to the combined statement instead would look
	// equivalent and is not: those values were read moments earlier, so
	// re-writing them would silently revert whatever the API consumer wrote in
	// between. Not naming the columns removes that window rather than narrowing
	// it.
	if _, owned := lookupCopilotAPIState(sess.ConvID); owned {
		if snapshot.Model == stored.Model && snapshot.ReasoningEffort == stored.EffortLevel {
			return
		}
		if _, err := db.UpdateModelEffortForGeneration(
			sess.ID, sess.ConvID, sess.CreatedAt, snapshot.Model, snapshot.ReasoningEffort,
		); err != nil {
			slog.Warn("copilot-usage: failed to persist model and effort",
				"session_id", sess.ID, "error", err, "module", "agentd")
		}
		return
	}

	window := copilotEffectiveContextWindow(sess.ConvID, snapshot.Model, stored.ContextWindowSize)
	pct := copilotContextPct(snapshot.LastCallInputTokens, window)

	// tokens_output may only ever ADVANCE, hence max() rather than the sweep's
	// own figure. The two sources count different things and either may be
	// ahead: the durable log's shutdown total is session-lifetime and is
	// restored across a resume, while the sweep counts only the rows it has
	// consumed — which is legitimately behind mid-drain of a large backlog, and
	// starts from zero for a session first seen after a resume.
	//
	// Writing the lower figure would not merely blink: it would STICK. The
	// read-through follower skips its write when its projection matches its own
	// persisted mirror, and that mirror still holds the higher value, so it
	// never issues the corrective write. The regression is only observable in
	// the row, which is exactly where it would be seen.
	output := max(snapshot.OutputTokens, stored.TokensOutput)

	if pct == stored.ContextPct &&
		snapshot.LastCallInputTokens == stored.TokensInput &&
		output == stored.TokensOutput &&
		snapshot.Model == stored.Model &&
		snapshot.ReasoningEffort == stored.EffortLevel {
		return
	}
	if _, err := db.UpdateContextSnapshotAndModelEffortForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt,
		pct, snapshot.LastCallInputTokens, output, stored.ContextWindowSize,
		snapshot.Model, snapshot.ReasoningEffort,
	); err != nil {
		slog.Warn("copilot-usage: failed to persist context snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
	}
}

// copilotLaunchIntent is the durable Copilot launch intent a conversation was
// started with: the configured context cap and which drive it runs on. Both are
// tclaude intent with no sessions column of their own, so both live in the same
// two durable records — the stable agent relaunch profile, then the
// conversation fallback that keeps legacy/pending conversations answerable until
// that agent row exists.
//
// They are read TOGETHER on purpose. The dashboard asks for both on every
// snapshot tick for every Copilot agent, and resolving them separately doubled
// the durable-record reads per agent per tick for no benefit.
//
// Zero means no explicit cap, leaving the caller free to pick the observed
// model's static default; false means the tmux send-keys drive, which is what
// every Copilot agent ran before the drive was selectable.
type copilotLaunchIntent struct {
	ContextWindowMax int64
	API              bool
}

func copilotLaunchIntentForConv(convID string) copilotLaunchIntent {
	copilot := &harness.Harness{Name: harness.CopilotName}
	var out copilotLaunchIntent
	var haveMax, haveAPI bool

	// Highest-precedence record first, then the fallback fills only what the
	// agent profile left unrecorded — so a managed agent's frozen posture always
	// wins field-by-field, matching composeAgentRelaunchProfile's ordering.
	apply := func(profile *db.AgentRelaunchProfile) {
		if profile == nil {
			return
		}
		if !haveMax && profile.ConfiguredContextWindowMax != nil {
			if value, err := harness.ResolveCopilotContextWindow(
				copilot, *profile.ConfiguredContextWindowMax); err == nil {
				out.ContextWindowMax, haveMax = value, true
			}
		}
		if !haveAPI && profile.CopilotAPI != nil {
			if value, err := harness.ResolveCopilotAPI(copilot, profile.CopilotAPI); err == nil {
				out.API, haveAPI = value, true
			}
		}
	}

	if profile, err := db.AgentRelaunchProfileForConv(convID); err == nil {
		apply(profile)
	}
	if haveMax && haveAPI {
		return out
	}
	if conversation, err := db.ConversationResumeProfileForConv(convID); err == nil &&
		conversation != nil {
		apply(conversation.FallbackRelaunch)
	}
	return out
}

// copilotAPIPostureRecorded reports whether ANY record answers "which drive did
// this conversation's launch take".
//
// It exists because [copilotLaunchIntentForConv] deliberately cannot answer it:
// its API field is false both for a launch that chose send-keys and for a launch
// that recorded nothing, and collapsing those is safe for ROUTING (both mean
// "do not assume the API channel") but useless as a signal. A conversation with
// no record at all is a different thing entirely — since TCL-1059 closed every
// path that mints a conv id, it means either a genuinely legacy conversation or
// a NEW launch path nobody has taught to record the posture, and the second is
// a regression that is otherwise completely silent.
//
// Kept beside the intent read, and reading the same two records in the same
// precedence, so a future change to that precedence cannot leave this one
// answering about a record set the router no longer consults.
//
// Nothing may ROUTE on this. It is an observation for logs.
func copilotAPIPostureRecorded(convID string) bool {
	if profile, err := db.AgentRelaunchProfileForConv(convID); err == nil &&
		profile != nil && profile.CopilotAPI != nil {
		return true
	}
	conversation, err := db.ConversationResumeProfileForConv(convID)
	return err == nil && conversation != nil && conversation.FallbackRelaunch != nil &&
		conversation.FallbackRelaunch.CopilotAPI != nil
}

// copilotConfiguredContextWindowMax returns only the configured cap, for the
// usage paths that have no use for the drive.
func copilotConfiguredContextWindowMax(convID string) int64 {
	return copilotLaunchIntentForConv(convID).ContextWindowMax
}

// copilotEffectiveContextWindow is the ONE place the meter's effective
// denominator is resolved, shared by the sweep and the read-through follower
// so the two writers converge on the same percentage instead of flapping the
// row (the follower once recomputed against only the observed column, which
// Copilot rarely discloses, and overwrote the sweep's percentage with 0 on
// every token advance).
//
// The order is the settled TCL-1048 precedence, then the observed disclosure
// as the last resort: an explicit configured cap is operator intent and wins
// outright; a fresh remote catalog entry outranks the static per-model
// fallback; a window Copilot itself disclosed in the durable log is used only
// when tclaude knows nothing better. This matches the dashboard tooltip's own
// fallback (configured/catalog/assumed max, else the observed window), so the
// percentage and the "x / y tokens" beside it always describe the same ratio.
func copilotEffectiveContextWindow(convID, model string, observed int64) int64 {
	if window := copilotConfiguredContextWindowMax(convID); window > 0 {
		return window
	}
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		if window := copilotCatalogContextWindow(trimmed); window > 0 {
			return window
		}
		if window := harness.CopilotContextWindowDefault(trimmed); window > 0 {
			return window
		}
	}
	return max(observed, 0)
}

// copilotContextPct is the ONE place Copilot's occupancy percentage is
// computed, shared by the sweep and the read-through follower so the two can
// never write different percentages for the same observation.
//
// 0 means unknown, matching CopilotContextTelemetry.Pct and every other
// harness's snapshot.
func copilotContextPct(currentTokens, window int64) float64 {
	if currentTokens <= 0 || window <= 0 {
		return 0
	}
	return float64(currentTokens) / float64(window) * 100
}

func isCopilotUsageStoreAbsent(err error) bool {
	return errors.Is(err, harness.ErrCopilotUsageStoreAbsent)
}
