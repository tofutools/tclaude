package agentd

import (
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
const brokerRefusalWindow = 15 * time.Minute

// brokerRefusalPruneEvery bounds how often the recorder sweeps expired
// entries. Refusals are rare by construction, so this only guards
// against a pathological caller growing the map without bound.
const brokerRefusalPruneEvery = 256

type brokerRefusal struct {
	Count  int
	First  time.Time
	Last   time.Time
	Reason string
}

type brokerRefusalRecorder struct {
	mu sync.Mutex
	// bySession is keyed by the DAEMON-RESOLVED session row id. Never by
	// anything the caller sent.
	bySession map[string]*brokerRefusal
	// unplaceable counts refusals with no row to attribute to.
	unplaceable brokerRefusal
	writes      int
	now         func() time.Time
}

var brokerRefusals = &brokerRefusalRecorder{bySession: map[string]*brokerRefusal{}}

func (r *brokerRefusalRecorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// recordClaimMismatch attributes a refusal to the row the daemon itself
// resolved for the caller. sessionID must come from the ancestry walk.
func (r *brokerRefusalRecorder) recordClaimMismatch(sessionID, reason string) {
	if sessionID == "" {
		r.recordUnplaceable(reason)
		return
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
}

func (r *brokerRefusalRecorder) recordUnplaceable(reason string) {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unplaceable.Count == 0 || now.Sub(r.unplaceable.Last) > brokerRefusalWindow {
		r.unplaceable = brokerRefusal{First: now}
	}
	r.unplaceable.Count++
	r.unplaceable.Last = now
	r.unplaceable.Reason = reason
}

func (r *brokerRefusalRecorder) pruneLocked(now time.Time) {
	for k, e := range r.bySession {
		if now.Sub(e.Last) > brokerRefusalWindow {
			delete(r.bySession, k)
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

// unplaceableCount reports refusals with no row to attribute to, or 0
// once the run has aged out. The snapshot handler wants only this, and it
// runs on the dashboard's 2s poll — no reason to copy the per-session map
// every tick to read one number.
func (r *brokerRefusalRecorder) unplaceableCount() int {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unplaceable.Count == 0 || now.Sub(r.unplaceable.Last) > brokerRefusalWindow {
		return 0
	}
	return r.unplaceable.Count
}

// snapshot returns the whole recorder state for the dashboard: the
// per-session records still inside the window, and the unplaceable
// count.
func (r *brokerRefusalRecorder) snapshot() (map[string]brokerRefusal, brokerRefusal) {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]brokerRefusal, len(r.bySession))
	for k, e := range r.bySession {
		if now.Sub(e.Last) <= brokerRefusalWindow {
			out[k] = *e
		}
	}
	unplaceable := r.unplaceable
	if unplaceable.Count > 0 && now.Sub(unplaceable.Last) > brokerRefusalWindow {
		unplaceable = brokerRefusal{}
	}
	return out, unplaceable
}

// resetForTest clears recorded refusals. The recorder is process-wide
// (one daemon, one view of the condition), so a test that drives a
// refusal has to start from a clean slate.
func (r *brokerRefusalRecorder) resetForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bySession = map[string]*brokerRefusal{}
	r.unplaceable = brokerRefusal{}
	r.writes = 0
}
