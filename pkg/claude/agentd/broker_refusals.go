package agentd

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- brokered-request refusals, recorded for the dashboard (TCL-761) ---
//
// A `tclaude-layer` agent reaches the conversation database only through
// the brokered endpoints. When the daemon refuses one of those requests
// the agent loses that telemetry — and in the pathological case it loses
// ALL of it, for its whole life, silently: status, ledgers, directory
// tracking and the context snapshot never update. The shipped mitigation
// was an ERROR in the agent's own log, which is detection for somebody
// already reading logs, not surfacing.
//
// This records refusals so the dashboard can show the condition. Two
// properties are load-bearing, both operator rulings:
//
//  1. NOTHING HERE IS CALLER-ASSERTED. A refused request carries a
//     claimed_session_id, and it is tempting to attribute the refusal to
//     the row it names — that IS the starved agent. It is also a string
//     the caller chose, and the entire reason the request was refused is
//     that we do not trust it. Attributing from it would let any wrapped
//     agent paint a warning on a PEER's row. The dashboard is how the
//     operator decides where to look, so a false signal there is not
//     merely cosmetic.
//
//     The two refusal cases are therefore asymmetric:
//     - CLAIM MISMATCH: identity DID resolve. The row is the daemon's own
//     conclusion, so the refusal is attributed to it. This is also the
//     common misconfiguration, so making it directly visible is the
//     point.
//     - UNPLACEABLE: no row resolved, so there is nothing trustworthy to
//     attribute to. Counted only.
//
//  2. A refusal stops counting once the agent that made it is gone. The
//     recorder notes the tmux session that owns the refusal and counts it
//     only while tmux still lists that session with a live pane. When an
//     agent is retired or deleted its session goes away and nothing is
//     being lost any more; keeping the warning up for the rest of the
//     window would send the operator looking for an agent that no longer
//     exists. A refusal whose owner could not be determined keeps counting
//     for the window. See brokerCallerOwner for how the owner is found —
//     deliberately without matching process names, which differ per
//     harness and change with every harness version.
//
//  3. State is in memory and resets with the daemon, like the rate
//     limiter's. These are operator hints, not an audit trail; persisting
//     them would put database writes on the failure path of the very
//     mechanism that exists because the database is unreachable.

// brokerRefusalWindow is how long a refusal keeps counting towards the
// visible condition. Long enough to survive an idle agent's quiet
// stretch, short enough that a condition an operator has fixed stops
// being shown without needing a daemon restart.
//
// The dashboard notice names this figure in prose ("in the last 15
// minutes", groups-island.js) because a count with no window is not
// interpretable. Change one, change the other.
const brokerRefusalWindow = 15 * time.Minute

// brokerRefusalPruneEvery bounds how often the recorder sweeps expired
// entries. Refusals are rare by construction, so this only guards
// against a pathological caller growing the map without bound.
const brokerRefusalPruneEvery = 256

// brokerRefusalLogInterval throttles the log line a refusal produces. A
// statusline renders several times a second, so an agent whose every
// callback is refused would otherwise write a WARN per render for the rest
// of its life — the same reasoning as brokerLimitLogInterval. The first
// refusal of a run is always logged; later ones within the interval are
// counted and reported as `suppressed` on the next line.
const brokerRefusalLogInterval = 10 * time.Second

type brokerRefusal struct {
	Count  int
	First  time.Time
	Last   time.Time
	Reason string
	// lastLogAt and suppressed drive the per-run log throttle. They are not
	// exposed on the dashboard; the copy forSession hands out carries them
	// only because it is a whole-struct copy.
	lastLogAt  time.Time
	suppressed int
	// owners counts this entry's refusals per owning tmux session ("" when
	// no owner could be determined). Only refusals whose owner is still
	// live are shown; see liveCount.
	owners map[string]int
}

// noteOwnerLocked records one refusal against the session that owns it.
func (e *brokerRefusal) noteOwnerLocked(owner string) {
	if e.owners == nil {
		e.owners = map[string]int{}
	}
	e.owners[owner]++
}

// hasOwnersLocked reports whether any refusal of e has a known owner, i.e.
// whether liveCount needs the pane table at all.
func (e *brokerRefusal) hasOwnersLocked() bool {
	for owner := range e.owners {
		if owner != "" {
			return true
		}
	}
	return false
}

// liveCount is Count minus the refusals whose owning tmux session is gone
// or whose pane is dead in panes. An unknown owner ("") is assumed live, and
// so is every owner when panes is nil (tmux could not be read): a failed
// read must not hide a real condition.
func (e *brokerRefusal) liveCount(panes map[string]brokerPaneEntry) int {
	n := e.Count
	if panes == nil {
		return n
	}
	for owner, c := range e.owners {
		if owner == "" {
			continue
		}
		if p, ok := panes[owner]; !ok || p.dead {
			n -= c
		}
	}
	return n
}

// brokerRefusalLogDecision is what a record call tells its caller about
// logging: whether to write a line now, and how many refusals of this run
// went unlogged since the previous line.
type brokerRefusalLogDecision struct {
	Log        bool
	Suppressed int
	// Count is the run's total so far, for the log line.
	Count int
}

// noteLogLocked applies the throttle to one entry and returns the decision.
func (e *brokerRefusal) noteLogLocked(now time.Time) brokerRefusalLogDecision {
	if !e.lastLogAt.IsZero() && now.Sub(e.lastLogAt) < brokerRefusalLogInterval {
		e.suppressed++
		return brokerRefusalLogDecision{Count: e.Count}
	}
	d := brokerRefusalLogDecision{Log: true, Suppressed: e.suppressed, Count: e.Count}
	e.lastLogAt = now
	e.suppressed = 0
	return d
}

type brokerRefusalRecorder struct {
	mu sync.Mutex
	// bySession is keyed by the DAEMON-RESOLVED session row id. Never by
	// anything the caller sent.
	bySession map[string]*brokerRefusal
	// unplaceable counts refusals with no row to attribute to. It is the
	// daemon-wide figure the dashboard counter shows.
	unplaceable brokerRefusal
	// unplaceableByCaller drives the LOG throttle for unplaceable refusals,
	// keyed by the socket peer's pid — a kernel fact, not a caller string.
	// One shared bucket would let a chatty orphan's renders swallow the
	// first (and only) refusal of a different caller, and would make the
	// suppressed count on a line name one caller while counting another.
	// The pid is a log key only; it never contributes to attribution.
	unplaceableByCaller map[int]*brokerRefusal
	writes              int
	now                 func() time.Time
	// panes reads tmux's pane table for owner liveness. nil means the
	// shared broker pane cache (cachedBrokerPanes): one `list-panes -a` per
	// brokerPaneCacheTTL daemon-wide, shared with the identity proof.
	panes func() map[string]brokerPaneEntry
	// panesFailedAt backs off pane-table reads after a failed one, for
	// brokerPaneCacheTTL. The shared cache keeps no record of a failure, so
	// without this a wedged tmux would cost every refusal and every
	// dashboard poll a fresh list-panes of up to the tmux timeout.
	// panesLoading/panesDone single-flight the read here too: callers
	// queued behind a hanging read would otherwise each become the next
	// loader of the shared cache once it fails. Guarded by panesMu, never
	// by mu.
	// dismissedAt is when the operator last dismissed the machine-level
	// notice. counts() reports nothing until a refusal newer than it
	// arrives, so a dismissal silences the instance on screen without
	// hiding a later one. Per-row badges are unaffected. Guarded by mu.
	dismissedAt time.Time

	panesMu       sync.Mutex
	panesFailedAt time.Time
	panesLoading  bool
	panesDone     chan struct{}
}

var brokerRefusals = newBrokerRefusalRecorder(nil)

func newBrokerRefusalRecorder(now func() time.Time) *brokerRefusalRecorder {
	return &brokerRefusalRecorder{
		bySession:           map[string]*brokerRefusal{},
		unplaceableByCaller: map[int]*brokerRefusal{},
		now:                 now,
	}
}

func (r *brokerRefusalRecorder) paneTable() map[string]brokerPaneEntry {
	for {
		r.panesMu.Lock()
		if !r.panesFailedAt.IsZero() && time.Since(r.panesFailedAt) < brokerPaneCacheTTL {
			r.panesMu.Unlock()
			return nil
		}
		if r.panesLoading {
			done := r.panesDone
			r.panesMu.Unlock()
			<-done
			// Re-check: a failed read puts every waiter into backoff; a
			// successful one left a fresh shared cache to read cheaply.
			continue
		}
		r.panesLoading = true
		r.panesDone = make(chan struct{})
		done := r.panesDone
		r.panesMu.Unlock()
		return r.loadPaneTable(done)
	}
}

// loadPaneTable runs the single-flighted read and releases its waiters,
// even if the read panics, so no later caller blocks on done forever.
func (r *brokerRefusalRecorder) loadPaneTable(done chan struct{}) (table map[string]brokerPaneEntry) {
	defer func() {
		r.panesMu.Lock()
		if table == nil {
			r.panesFailedAt = time.Now()
		}
		r.panesLoading = false
		close(done)
		r.panesMu.Unlock()
	}()
	read := cachedBrokerPanes
	if r.panes != nil {
		read = r.panes
	}
	return read()
}

// refusalOwner resolves ctx's owner, reading the pane table only when the
// answer can depend on it: with neither a process table to walk nor a
// claim to fall back on, the owner is unknown whatever tmux says. The
// pre-identity rate-limit refusals take that path on purpose — they exist
// to shed a flood cheaply and must not cost a tmux read.
func (r *brokerRefusalRecorder) refusalOwner(ctx brokerRefusalContext) string {
	if ctx.procs == nil && ctx.claimedTmux == "" {
		return ""
	}
	return brokerCallerOwner(ctx.procs, r.paneTable(), ctx.CallerPID, ctx.claimedTmux)
}

func (r *brokerRefusalRecorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// recordClaimMismatch attributes a refusal to the row the daemon itself
// resolved for the caller. sessionID must come from the ancestry walk.
func (r *brokerRefusalRecorder) recordClaimMismatch(sessionID, reason string) brokerRefusalLogDecision {
	return r.recordClaimMismatchOwned(sessionID, "", reason)
}

// recordClaimMismatchOwned is recordClaimMismatch with the tmux session
// that owns the refusal ("" when unknown); see brokerCallerOwner.
func (r *brokerRefusalRecorder) recordClaimMismatchOwned(sessionID, owner, reason string) brokerRefusalLogDecision {
	if sessionID == "" {
		return r.recordUnplaceableOwned(0, owner, reason)
	}
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()

	e := r.bySession[sessionID]
	if e == nil || now.Sub(e.Last) > brokerRefusalWindow {
		e = &brokerRefusal{First: now}
		r.bySession[sessionID] = e
	}
	e.Count++
	e.Last = now
	e.Reason = reason
	e.noteOwnerLocked(owner)

	r.writes++
	if r.writes%brokerRefusalPruneEvery == 0 {
		r.pruneLocked(now)
	}
	return e.noteLogLocked(now)
}

func (r *brokerRefusalRecorder) recordUnplaceable(reason string) brokerRefusalLogDecision {
	return r.recordUnplaceableFor(0, reason)
}

func (r *brokerRefusalRecorder) recordUnplaceableFor(callerPID int, reason string) brokerRefusalLogDecision {
	return r.recordUnplaceableOwned(callerPID, "", reason)
}

// recordUnplaceableOwned is recordUnplaceable with the caller pid the log
// throttle is keyed by, and the owning tmux session ("" when unknown). The
// dashboard counter stays daemon-wide; only the decision about writing a
// log line, and the counts that line carries, are per caller. callerPID 0
// (unknown) shares one bucket.
func (r *brokerRefusalRecorder) recordUnplaceableOwned(callerPID int, owner, reason string) brokerRefusalLogDecision {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unplaceable.Count == 0 || now.Sub(r.unplaceable.Last) > brokerRefusalWindow {
		r.unplaceable = brokerRefusal{First: now}
	}
	r.unplaceable.Count++
	r.unplaceable.Last = now
	r.unplaceable.Reason = reason
	r.unplaceable.noteOwnerLocked(owner)

	if r.unplaceableByCaller == nil {
		r.unplaceableByCaller = map[int]*brokerRefusal{}
	}
	e := r.unplaceableByCaller[callerPID]
	if e == nil || now.Sub(e.Last) > brokerRefusalWindow {
		e = &brokerRefusal{First: now}
		r.unplaceableByCaller[callerPID] = e
	}
	e.Count++
	e.Last = now
	e.Reason = reason

	r.writes++
	if r.writes%brokerRefusalPruneEvery == 0 {
		r.pruneLocked(now)
	}
	return e.noteLogLocked(now)
}

// brokerClaimReason picks the reason for a refusal that fell through to a
// "no claim" branch. When the caller DID send a claim but the proof could
// not even load the claimed row (proofDetail set with layerClaim false —
// a database error, typically SQLITE_BUSY), saying the caller "omitted"
// its claim would send the operator to the wrong place: the badge text on
// the dashboard is this same string. The endpoint prefix is kept.
func brokerClaimReason(fallback, claimed, proofDetail string) string {
	if claimed == "" || proofDetail == "" {
		return fallback
	}
	prefix := fallback
	if i := strings.Index(fallback, ":"); i >= 0 {
		prefix = fallback[:i]
	}
	return prefix + ": claimed session could not be loaded to check the claim"
}

// brokerCallerGoneKey namespaces the caller-gone log throttle in the broker
// limiter's bucket map; the NUL prefix cannot collide with a session id.
const brokerCallerGoneKey = "\x00caller-gone"

// noteCallerGone logs a brokered request whose caller process had already
// exited — the client's own timeout fired while the daemon was busy — and
// deliberately records NO refusal: the badge and counter mean an agent's
// telemetry is stuck, and a render the client retries is not that. It is
// still a WARN, because a daemon losing this race is a daemon that is too
// slow, and the operator should see that with the caller pid and the row
// it concerned. Throttled per resolved row (one bucket for unplaced
// callers) like the refusal line, since a busy daemon loses the race in
// bursts.
func (r *brokerRefusalRecorder) noteCallerGone(ctx brokerRefusalContext) {
	key := brokerCallerGoneKey
	if ctx.Resolved != "" {
		key += ":row:" + ctx.Resolved
	}
	defaultBrokerLimiter.observe(key, 0)
	log, suppressed := defaultBrokerLimiter.shouldLogExcess(key)
	if !log {
		return
	}
	attrs := []any{
		"endpoint", ctx.Endpoint,
		"caller_pid", ctx.CallerPID,
		"resolved_session", ctx.Resolved,
		"claimed_session", auditClip(ctx.Claimed, brokerRefusalLogClip),
		"suppressed_since_last_log", suppressed - 1,
	}
	if ctx.Detail != "" {
		attrs = append(attrs, "detail", ctx.Detail)
	}
	if ctx.Event != "" {
		attrs = append(attrs, "event", auditClip(ctx.Event, brokerRefusalLogClip))
	}
	attrs = append(attrs, "module", "hooks")
	slog.Warn("broker: caller exited before its callback could be verified; "+
		"the client timed out while the daemon was busy and will retry, so nothing is badged", attrs...)
}

// brokerRefusalContext is what a refusal log line needs beyond the
// recorder's own decision. Every field is daemon-derived except Claimed,
// which is the caller's own string and labelled as such in the log.
type brokerRefusalContext struct {
	Endpoint  string
	CallerPID int
	// Resolved is the row the daemon's ancestry walk concluded, or "" when
	// nothing resolved. It is also the row the refusal was attributed to.
	Resolved string
	// Claimed is the session id the caller sent. Logged for correlation
	// only; it never selects the row the refusal is recorded against.
	Claimed string
	// Detail is the proof's own account of what was missing
	// (proveTclaudeLayerCallerDetailed), or "" when there is none.
	Detail string
	// Event is the hook event name, or "" for the statusline endpoint.
	Event string
	// procs is the request's process-table view, used to find the pane
	// that owns the refusal. nil leaves the owner to the claim alone.
	procs *brokerProcTable
	// claimedTmux is the tmux session recorded on the CLAIMED row, or ""
	// when the claim loaded no row. Like Claimed it is caller-selected, so
	// it is only ever an owner for liveness; see brokerCallerOwner.
	claimedTmux string
}

// brokerCallerOwnerMaxDepth bounds the ancestry walk in brokerCallerOwner.
const brokerCallerOwnerMaxDepth = 64

// brokerCallerOwner names the tmux session whose lifetime bounds a refusal:
// once tmux no longer lists it with a live pane, the agent behind the
// refusal is gone and the refusal stops counting. "" means unknown, which
// keeps counting for the window.
//
// Nothing here matches process names. A sandboxed Claude Code runs as its
// version string (e.g. "2.1.295"), every harness names itself differently,
// and versions change constantly; the identity proof stopped relying on
// names for the same reason. The facts used instead:
//
//  1. The nearest ancestor of the caller that tmux reports as a pane pid.
//     That is the agent's own pane, whatever runs inside it.
//  2. Failing that, the tmux session of the row the caller CLAIMED. This
//     is the teardown case: a retired agent's last SessionEnd hook arrives
//     after its tmux session was killed, so no live pane is in its
//     ancestry. The claim is caller-chosen, but here it can only decide
//     whether the caller's OWN refusal keeps counting — never which row is
//     badged — and a caller that wanted to hide could simply stay silent.
//     A live agent whose claim names a live session still counts.
//
// When tmux's pane table cannot be read, the walk has nothing to match and
// the claim alone decides. Returning "" there instead would make the owner
// unknown for good — the owner is fixed at record time, so a refusal noted
// during one failed list-panes would keep counting for the whole window even
// after tmux recovers and plainly no longer lists the claimed session. That
// is exactly the retire case: the last SessionEnd hook lands while the pane is
// being torn down. A nil table at count time still counts everything
// (liveCount), so this hides nothing while tmux stays unreadable.
//
// Cost: the walk reads the request's own process table, which the identity
// walk has already filled for these pids (on macOS from the shared ps
// snapshot), and the pane table is the shared list-panes cache the proof
// itself reads. It runs only on a refusal.
func brokerCallerOwner(t *brokerProcTable, panes map[string]brokerPaneEntry, callerPID int, claimedTmux string) string {
	if t != nil && panes != nil {
		byPID := make(map[int]string, len(panes))
		for name, p := range panes {
			if p.panePID > 1 {
				byPID[p.panePID] = name
			}
		}
		cur := callerPID
		for i := 0; cur > 1 && i < brokerCallerOwnerMaxDepth; i++ {
			if name, ok := byPID[cur]; ok {
				return name
			}
			cur = t.parent(cur)
		}
	}
	return claimedTmux
}

// refuseAttributed records a refusal against the daemon-resolved row AND
// writes the log line the dashboard notice promises ("the daemon log has
// the caller pid"). The record and the log are one call on purpose: every
// refusal branch used to record for the dashboard and then return silently,
// so the badge said something was wrong and nothing said what.
func (r *brokerRefusalRecorder) refuseAttributed(reason string, ctx brokerRefusalContext) {
	owner := r.refusalOwner(ctx)
	r.logRefusal(r.recordClaimMismatchOwned(ctx.Resolved, owner, reason), reason, owner, ctx)
}

// refuseUnplaceable is refuseAttributed for a caller no row resolved for.
func (r *brokerRefusalRecorder) refuseUnplaceable(reason string, ctx brokerRefusalContext) {
	ctx.Resolved = ""
	owner := r.refusalOwner(ctx)
	r.logRefusal(r.recordUnplaceableOwned(ctx.CallerPID, owner, reason), reason, owner, ctx)
}

func (r *brokerRefusalRecorder) logRefusal(d brokerRefusalLogDecision, reason, owner string, ctx brokerRefusalContext) {
	if !d.Log {
		return
	}
	attrs := []any{
		"endpoint", ctx.Endpoint,
		"caller_pid", ctx.CallerPID,
		"owner_tmux_session", owner,
		"reason", reason,
		"resolved_session", ctx.Resolved,
		"claimed_session", auditClip(ctx.Claimed, brokerRefusalLogClip),
		"refusals_in_window", d.Count,
		"suppressed_since_last_log", d.Suppressed,
		"window", brokerRefusalWindow.String(),
	}
	if ctx.Detail != "" {
		attrs = append(attrs, "detail", ctx.Detail)
	}
	if ctx.Event != "" {
		// Also caller-supplied (it is a field of the hook payload).
		attrs = append(attrs, "event", auditClip(ctx.Event, brokerRefusalLogClip))
	}
	attrs = append(attrs, "module", "hooks")
	slog.Warn("broker: refused a brokered callback; the caller's telemetry for this request is lost", attrs...)
}

func (r *brokerRefusalRecorder) pruneLocked(now time.Time) {
	for k, e := range r.bySession {
		if now.Sub(e.Last) > brokerRefusalWindow {
			delete(r.bySession, k)
		}
	}
	for k, e := range r.unplaceableByCaller {
		if now.Sub(e.Last) > brokerRefusalWindow {
			delete(r.unplaceableByCaller, k)
		}
	}
}

// forSession reports the live refusal record for a resolved session row,
// or nil when there is none inside the window. Count is the live count:
// refusals whose owning agent is gone are left out.
func (r *brokerRefusalRecorder) forSession(sessionID string) *brokerRefusal {
	if sessionID == "" {
		return nil
	}
	now := r.clock()
	r.mu.Lock()
	e := r.bySession[sessionID]
	if e == nil || now.Sub(e.Last) > brokerRefusalWindow {
		r.mu.Unlock()
		return nil
	}
	cp := *e
	cp.owners = make(map[string]int, len(e.owners))
	for k, v := range e.owners {
		cp.owners[k] = v
	}
	needPanes := e.hasOwnersLocked()
	r.mu.Unlock()

	// The pane table is read outside the lock: a refresh is a tmux call,
	// and refusal recording must never wait behind one.
	var panes map[string]brokerPaneEntry
	if needPanes {
		panes = r.paneTable()
	}
	cp.Count = cp.liveCount(panes)
	cp.owners = nil
	if cp.Count <= 0 {
		return nil
	}
	return &cp
}

// counts reports every refusal inside the window whose owning harness is
// still running, and the unplaceable subset of it. The snapshot handler wants only these two numbers, and it
// runs on the dashboard's 2s poll — no reason to copy the per-session map
// every tick.
//
// total exists because a per-agent badge can be recorded and then rendered
// nowhere: the row a refusal is attributed to is by construction sometimes
// a dead one, and an operator who hides offline agents (or whose conv was
// never grouped) would see no badge and no counter, i.e. exactly the
// silence this feature exists to break. A machine-level total cannot say
// WHICH agent — that would need the attribution the rulings forbid — but
// it can say the condition is happening, which is enough to go looking.
//
// The pane table is only read when a refusal with a known owner is inside
// the window, so a quiet daemon's poll costs no tmux call; when one is, it
// is the shared cache (at most one list-panes per brokerPaneCacheTTL), read
// outside the lock.
func (r *brokerRefusalRecorder) counts() (total, unplaceable int) {
	now := r.clock()
	inWindow := func(e *brokerRefusal) bool {
		return e.Count > 0 && now.Sub(e.Last) <= brokerRefusalWindow
	}
	r.mu.Lock()
	needPanes := inWindow(&r.unplaceable) && r.unplaceable.hasOwnersLocked()
	for _, e := range r.bySession {
		if needPanes {
			break
		}
		needPanes = inWindow(e) && e.hasOwnersLocked()
	}
	r.mu.Unlock()

	var panes map[string]brokerPaneEntry
	if needPanes {
		panes = r.paneTable()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var latest time.Time
	for _, e := range r.bySession {
		if inWindow(e) {
			if n := e.liveCount(panes); n > 0 {
				total += n
				if e.Last.After(latest) {
					latest = e.Last
				}
			}
		}
	}
	if inWindow(&r.unplaceable) {
		unplaceable = r.unplaceable.liveCount(panes)
		total += unplaceable
		if unplaceable > 0 && r.unplaceable.Last.After(latest) {
			latest = r.unplaceable.Last
		}
	}
	if !r.dismissedAt.IsZero() && !latest.After(r.dismissedAt) {
		return 0, 0
	}
	return total, unplaceable
}

// dismiss silences the machine-level notice currently on screen. It comes
// back with the next refusal recorded after this moment.
func (r *brokerRefusalRecorder) dismiss() {
	now := r.clock()
	r.mu.Lock()
	r.dismissedAt = now
	r.mu.Unlock()
}

// handleDashboardBrokerRefusalsDismiss is the notice's dismiss button. Like
// every dashboard mutation it enforces the dashboard's cookie + Origin gate
// itself: a bodyless POST is a CORS simple request, so without it any page
// the operator visits could silence the warning.
func handleDashboardBrokerRefusalsDismiss(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	brokerRefusals.dismiss()
	w.WriteHeader(http.StatusNoContent)
}

// resetForTest clears recorded refusals. The recorder is process-wide
// (one daemon, one view of the condition), so a test that drives a
// refusal has to start from a clean slate.
func (r *brokerRefusalRecorder) resetForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bySession = map[string]*brokerRefusal{}
	r.unplaceable = brokerRefusal{}
	r.unplaceableByCaller = map[int]*brokerRefusal{}
	r.writes = 0
	r.dismissedAt = time.Time{}
	r.panesMu.Lock()
	r.panesFailedAt = time.Time{}
	r.panesMu.Unlock()
}

// brokerRefusalLogClip bounds caller-controlled strings in a refusal log
// line. The claimed id is bounded only by the request body cap, and a
// caller must not be able to turn one WARN per ten seconds into a
// multi-megabyte one. Same figure the audit log uses for such fields.
const brokerRefusalLogClip = 120
