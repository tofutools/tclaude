package agentd

import (
	"log/slog"
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
//  2. State is in memory and resets with the daemon, like the rate
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
}

var brokerRefusals = newBrokerRefusalRecorder(nil)

func newBrokerRefusalRecorder(now func() time.Time) *brokerRefusalRecorder {
	return &brokerRefusalRecorder{
		bySession:           map[string]*brokerRefusal{},
		unplaceableByCaller: map[int]*brokerRefusal{},
		now:                 now,
	}
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
	if sessionID == "" {
		return r.recordUnplaceable(reason)
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

	r.writes++
	if r.writes%brokerRefusalPruneEvery == 0 {
		r.pruneLocked(now)
	}
	return e.noteLogLocked(now)
}

func (r *brokerRefusalRecorder) recordUnplaceable(reason string) brokerRefusalLogDecision {
	return r.recordUnplaceableFor(0, reason)
}

// recordUnplaceableFor is recordUnplaceable with the caller pid the log
// throttle is keyed by. The dashboard counter stays daemon-wide; only the
// decision about writing a log line, and the counts that line carries,
// are per caller. callerPID 0 (unknown) shares one bucket.
func (r *brokerRefusalRecorder) recordUnplaceableFor(callerPID int, reason string) brokerRefusalLogDecision {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unplaceable.Count == 0 || now.Sub(r.unplaceable.Last) > brokerRefusalWindow {
		r.unplaceable = brokerRefusal{First: now}
	}
	r.unplaceable.Count++
	r.unplaceable.Last = now
	r.unplaceable.Reason = reason

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
}

// refuseAttributed records a refusal against the daemon-resolved row AND
// writes the log line the dashboard notice promises ("the daemon log has
// the caller pid"). The record and the log are one call on purpose: every
// refusal branch used to record for the dashboard and then return silently,
// so the badge said something was wrong and nothing said what.
func (r *brokerRefusalRecorder) refuseAttributed(reason string, ctx brokerRefusalContext) {
	r.logRefusal(r.recordClaimMismatch(ctx.Resolved, reason), reason, ctx)
}

// refuseUnplaceable is refuseAttributed for a caller no row resolved for.
func (r *brokerRefusalRecorder) refuseUnplaceable(reason string, ctx brokerRefusalContext) {
	ctx.Resolved = ""
	r.logRefusal(r.recordUnplaceableFor(ctx.CallerPID, reason), reason, ctx)
}

func (r *brokerRefusalRecorder) logRefusal(d brokerRefusalLogDecision, reason string, ctx brokerRefusalContext) {
	if !d.Log {
		return
	}
	attrs := []any{
		"endpoint", ctx.Endpoint,
		"caller_pid", ctx.CallerPID,
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
// or nil when there is none inside the window.
func (r *brokerRefusalRecorder) forSession(sessionID string) *brokerRefusal {
	if sessionID == "" {
		return nil
	}
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.bySession[sessionID]
	if e == nil || now.Sub(e.Last) > brokerRefusalWindow {
		return nil
	}
	cp := *e
	return &cp
}

// counts reports every refusal inside the window, and the unplaceable
// subset of it. The snapshot handler wants only these two numbers, and it
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
func (r *brokerRefusalRecorder) counts() (total, unplaceable int) {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.bySession {
		if now.Sub(e.Last) <= brokerRefusalWindow {
			total += e.Count
		}
	}
	if r.unplaceable.Count > 0 && now.Sub(r.unplaceable.Last) <= brokerRefusalWindow {
		unplaceable = r.unplaceable.Count
		total += unplaceable
	}
	return total, unplaceable
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
}

// brokerRefusalLogClip bounds caller-controlled strings in a refusal log
// line. The claimed id is bounded only by the request body cap, and a
// caller must not be able to turn one WARN per ten seconds into a
// multi-megabyte one. Same figure the audit log uses for such fields.
const brokerRefusalLogClip = 120
