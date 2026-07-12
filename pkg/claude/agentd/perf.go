package agentd

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// perf.go — in-memory wall-clock timing for the dashboard's polled
// endpoints (TCL-374). The 2s poll showed 100+ ms handler times in the
// field with no way to see where the time went; this records every
// polled request's duration (plus a per-phase breakdown for the big
// /api/snapshot handler) into a per-endpoint ring buffer that
// GET /api/perf serves back for inspection — the data source for a
// future dashboard Debug tab.
//
// Deliberately daemon-memory only: the numbers describe THIS daemon
// process, a restart resetting them is correct, and keeping SQLite out
// of the write path means the instrumentation can never contribute to
// the latency it exists to measure.

// perfRingCap bounds each endpoint's sample history. At the dashboard's
// 2s poll cadence 1024 samples ≈ 34 minutes of history, for a few
// hundred KB across all polled endpoints — enough to eyeball a
// distribution without any eviction policy beyond the ring itself.
const perfRingCap = 1024

// perfSlowLogMs is the threshold above which a completed request also
// emits a debug log with its phase breakdown — so a slow poll can be
// diagnosed from logs alone, without the /api/perf round-trip.
const perfSlowLogMs = 100

type perfPhase struct {
	Name string  `json:"name"`
	Ms   float64 `json:"ms"`
}

type perfSample struct {
	At      time.Time   `json:"at"`
	TotalMs float64     `json:"total_ms"`
	Phases  []perfPhase `json:"phases,omitempty"`
}

// perfRing is a fixed-capacity ring of samples, oldest overwritten
// first. Kept trivially simple: append until full, then wrap.
type perfRing struct {
	samples []perfSample
	next    int
	full    bool
}

func (r *perfRing) add(s perfSample) {
	if len(r.samples) < perfRingCap {
		r.samples = append(r.samples, s)
		return
	}
	r.samples[r.next] = s
	r.next = (r.next + 1) % perfRingCap
	r.full = true
}

// ordered returns the ring's samples oldest→newest.
func (r *perfRing) ordered() []perfSample {
	if !r.full {
		return append([]perfSample{}, r.samples...)
	}
	out := make([]perfSample, 0, len(r.samples))
	out = append(out, r.samples[r.next:]...)
	out = append(out, r.samples[:r.next]...)
	return out
}

var perfMu sync.Mutex
var perfRings = map[string]*perfRing{}

// perfReset discards every endpoint's recorded samples. The operator
// uses it (via POST /api/perf/reset) to start a fresh distribution
// after changing the setup under measurement — e.g. spawning or
// retiring a batch of agents — so the aggregates don't blend the
// before and after.
func perfReset() {
	perfMu.Lock()
	defer perfMu.Unlock()
	perfRings = map[string]*perfRing{}
}

func perfRecord(endpoint string, s perfSample) {
	perfMu.Lock()
	defer perfMu.Unlock()
	ring := perfRings[endpoint]
	if ring == nil {
		ring = &perfRing{}
		perfRings[endpoint] = ring
	}
	ring.add(s)
}

// perfSpan accumulates one request's phase marks. It belongs to a
// single request goroutine — no locking. A nil span is a valid no-op
// receiver so handlers can mark phases unconditionally whether or not
// they were invoked through withPerfTiming.
type perfSpan struct {
	start  time.Time
	last   time.Time
	phases []perfPhase
}

// mark closes the phase that started at the previous mark (or at
// request start) and names it.
func (s *perfSpan) mark(name string) {
	if s == nil {
		return
	}
	now := time.Now()
	s.phases = append(s.phases, perfPhase{Name: name, Ms: durMs(now.Sub(s.last))})
	s.last = now
}

func durMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

type perfSpanKey struct{}

// perfSpanFrom returns the request's timing span, or nil when the
// handler wasn't wrapped by withPerfTiming (nil is safe to mark on).
func perfSpanFrom(r *http.Request) *perfSpan {
	s, _ := r.Context().Value(perfSpanKey{}).(*perfSpan)
	return s
}

// withPerfTiming wraps a polled dashboard handler: it stamps a perfSpan
// into the request context (so the handler can mark named phases) and
// records the request's total wall-clock into the endpoint's ring when
// the handler returns. For a handler that marked phases, the remainder
// between its last mark and completion — JSON encode + socket write —
// is recorded as a final "write" phase.
func withPerfTiming(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		span := &perfSpan{start: time.Now()}
		span.last = span.start
		next(w, r.WithContext(context.WithValue(r.Context(), perfSpanKey{}, span)))
		if len(span.phases) > 0 {
			span.mark("write")
		}
		total := durMs(time.Since(span.start))
		perfRecord(endpoint, perfSample{At: span.start, TotalMs: total, Phases: span.phases})
		if total >= perfSlowLogMs {
			args := []any{"endpoint", endpoint, "total_ms", total, "module", "agentd"}
			for _, p := range span.phases {
				args = append(args, "phase_"+p.Name, p.Ms)
			}
			slog.Debug("dashboard poll exceeded slow threshold", args...)
		}
	}
}

// perfQuantiles is the aggregate block shared by the per-endpoint total
// and the per-phase rows of the /api/perf payload.
type perfQuantiles struct {
	Count int     `json:"count"`
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

type perfPhaseAggregate struct {
	Name string `json:"name"`
	perfQuantiles
}

type perfEndpointView struct {
	Endpoint string `json:"endpoint"`
	perfQuantiles
	Phases  []perfPhaseAggregate `json:"phases,omitempty"`
	Samples []perfSample         `json:"samples"`
}

// quantilesOf computes nearest-rank percentiles. values may arrive in
// any order and is sorted in place.
func quantilesOf(values []float64) perfQuantiles {
	q := perfQuantiles{Count: len(values)}
	if len(values) == 0 {
		return q
	}
	sort.Float64s(values)
	rank := func(p float64) float64 {
		i := int(float64(len(values))*p+0.5) - 1
		i = max(i, 0)
		i = min(i, len(values)-1)
		return values[i]
	}
	q.P50Ms = rank(0.50)
	q.P90Ms = rank(0.90)
	q.P99Ms = rank(0.99)
	q.MaxMs = values[len(values)-1]
	return q
}

// perfEndpointViewOf assembles one endpoint's /api/perf block from its
// ordered samples. sampleLimit > 0 trims the raw sample list to the
// most recent N (aggregates always cover the full ring).
func perfEndpointViewOf(endpoint string, samples []perfSample, sampleLimit int) perfEndpointView {
	totals := make([]float64, 0, len(samples))
	// Phase order: first-seen scanning newest→oldest, so the payload
	// leads with the phase set of the current handler version even if
	// older samples in the ring predate a phase rename.
	phaseOrder := []string{}
	phaseSeen := map[string]bool{}
	phaseValues := map[string][]float64{}
	for i := len(samples) - 1; i >= 0; i-- {
		totals = append(totals, samples[i].TotalMs)
		for _, p := range samples[i].Phases {
			if !phaseSeen[p.Name] {
				phaseSeen[p.Name] = true
				phaseOrder = append(phaseOrder, p.Name)
			}
			phaseValues[p.Name] = append(phaseValues[p.Name], p.Ms)
		}
	}
	view := perfEndpointView{Endpoint: endpoint, perfQuantiles: quantilesOf(totals)}
	for _, name := range phaseOrder {
		view.Phases = append(view.Phases, perfPhaseAggregate{Name: name, perfQuantiles: quantilesOf(phaseValues[name])})
	}
	if sampleLimit > 0 && len(samples) > sampleLimit {
		samples = samples[len(samples)-sampleLimit:]
	}
	view.Samples = samples
	return view
}

// handleDashboardPerf serves GET /api/perf — the recorded poll-timing
// distributions, one block per polled endpoint. Cookie-authed
// (dashboard-only), read-only. `?limit=N` caps the raw samples returned
// per endpoint (default 360 ≈ 12 min at the 2s poll; 0 = the full
// ring). Aggregates always cover every held sample regardless of limit.
func handleDashboardPerf(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	limit := max(atoiOr(r.URL.Query().Get("limit"), 360), 0)

	perfMu.Lock()
	ordered := make(map[string][]perfSample, len(perfRings))
	for endpoint, ring := range perfRings {
		ordered[endpoint] = ring.ordered()
	}
	perfMu.Unlock()

	endpoints := make([]string, 0, len(ordered))
	for endpoint := range ordered {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)
	// Empty slice (not nil) so the JSON is [] before any poll landed.
	views := []perfEndpointView{}
	for _, endpoint := range endpoints {
		views = append(views, perfEndpointViewOf(endpoint, ordered[endpoint], limit))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"endpoints":    views,
	})
}

// handleDashboardPerfReset serves POST /api/perf/reset — clears every
// endpoint's timing ring (see perfReset). Cookie-authed
// (dashboard-only). The method check lives here rather than in the mux
// pattern: a method-scoped pattern would send a GET to the "/"
// catch-all (a confusing 404) instead of a clean 405.
func handleDashboardPerfReset(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	perfReset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
