package agentd

import (
	"net/http"
	"sync"
	"time"
)

// Debounce timings for the dashboard event broadcaster — see
// docs/plans/TODO/med-prio/dashboard-realtime-push.md.
//
//   - eventsBroadcastMinQuiet: at least this long since the last
//     Publish() before the next fire. A burst of writes within the
//     window collapses into one fan-out.
//   - eventsBroadcastMaxWait: never wait longer than this since the
//     first Publish() of the current window. Caps the fan-out
//     interval during sustained churn (a heavy multi-agent burst
//     does not silence SSE indefinitely).
//
// Together they shape: an isolated write fires ~100ms after the call;
// a sustained stream fires at most ~once per second.
//
// Vars, not consts, so tests can shrink them.
var (
	eventsBroadcastMinQuiet = 100 * time.Millisecond
	eventsBroadcastMaxWait  = 1000 * time.Millisecond
)

// dashboardEvents is the process-wide broadcaster the SSE handler
// (handleDashboardEvents) reads from. Daemon writes that affect the
// dashboard call Publish(); each connected dashboard tab is one
// Subscribe()'r and receives a tick each time a debounce window
// settles.
//
// The SSE wire format carries no kind / no payload — clients react to
// every event by re-fetching /api/snapshot. Decoupling the event from
// the payload keeps the protocol stable across snapshot-shape changes.
var dashboardEvents = newBroadcaster()

// broadcaster fans Publish() calls out to Subscribe()'d channels,
// after a global debounce window. One goroutine + one timer at a
// time; all state runs under mu.
type broadcaster struct {
	mu       sync.Mutex
	subs     map[chan struct{}]struct{}
	pending  bool
	firstAt  time.Time
	lastAt   time.Time
	timer    *time.Timer
	minQuiet time.Duration
	maxWait  time.Duration
}

func newBroadcaster() *broadcaster {
	return &broadcaster{
		subs:     make(map[chan struct{}]struct{}),
		minQuiet: eventsBroadcastMinQuiet,
		maxWait:  eventsBroadcastMaxWait,
	}
}

// Publish enqueues an event. The actual fan-out to subscribers
// happens after the debounce window settles: at least minQuiet has
// elapsed since the most recent Publish, OR maxWait has elapsed
// since the first Publish in the current window — whichever comes
// first.
//
// Concurrent calls are safe; high-frequency calls coalesce.
func (b *broadcaster) Publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if !b.pending {
		b.pending = true
		b.firstAt = now
	}
	b.lastAt = now
	b.scheduleLocked(now)
}

// scheduleLocked (re)arms the fire timer based on current state.
// Called with mu held.
func (b *broadcaster) scheduleLocked(now time.Time) {
	quietDeadline := b.lastAt.Add(b.minQuiet)
	maxDeadline := b.firstAt.Add(b.maxWait)
	deadline := quietDeadline
	if maxDeadline.Before(deadline) {
		deadline = maxDeadline
	}
	d := max(deadline.Sub(now), 0)
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(d, b.fire)
}

// fire is the timer callback. If the debounce window has settled
// fans out to subscribers; otherwise reschedules. A stale fire
// (timer.Stop lost the race against a callback already in flight)
// shows up here as either pending=false (no-op) or window-not-yet-
// settled (reschedule) — both safe.
func (b *broadcaster) fire() {
	b.mu.Lock()
	if !b.pending {
		b.mu.Unlock()
		return
	}
	now := time.Now()
	quietDeadline := b.lastAt.Add(b.minQuiet)
	maxDeadline := b.firstAt.Add(b.maxWait)
	if now.Before(quietDeadline) && now.Before(maxDeadline) {
		b.scheduleLocked(now)
		b.mu.Unlock()
		return
	}
	b.pending = false
	snapshot := make([]chan struct{}, 0, len(b.subs))
	for c := range b.subs {
		snapshot = append(snapshot, c)
	}
	b.mu.Unlock()
	for _, c := range snapshot {
		// Non-blocking send: the subscriber's buffer is 1-deep and a
		// queued event already says "fresh data is waiting." A
		// subscriber lagging behind fan-outs will see one fewer
		// nudge, then re-fetch /api/snapshot on the next nudge it
		// does receive — strictly fewer round-trips, same end state.
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a channel that receives a value each time the
// broadcaster fires, plus a cancel func that unsubscribes. The
// channel buffer is 1; see fire's non-blocking-send comment.
func (b *broadcaster) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
	return ch, cancel
}

// publishOnSuccessfulWrite wraps an http.Handler so that a non-GET
// request returning 2xx triggers dashboardEvents.Publish(). Mounted
// at the mux level so every current and future mutation route picks
// the SSE hook up automatically — no risk of a new /api/foo POST
// shipping without firing dashboard updates.
//
// GET / HEAD / OPTIONS pass through unmodified. In particular, the
// SSE handler itself (GET /api/events) hits the unwrapped path, so
// the response writer it receives still satisfies http.Flusher.
//
// The broadcaster debounces, so even a burst of writes from one
// dashboard gesture (drag-rebroadcast, click-fire-N-mutations)
// collapses to one snapshot re-fetch on the browser side.
func publishOnSuccessfulWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		rec := &publishStatusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.code >= 200 && rec.code < 300 {
			dashboardEvents.Publish()
		}
	})
}

// publishStatusRecorder captures the response status code without
// otherwise altering the writer's behaviour. The default of 200
// matches net/http's implicit-WriteHeader contract: a handler that
// writes a body without calling WriteHeader has implicitly returned
// 200.
type publishStatusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (r *publishStatusRecorder) WriteHeader(c int) {
	if !r.wroteHeader {
		r.code = c
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(c)
}
