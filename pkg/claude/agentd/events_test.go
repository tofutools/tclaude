package agentd

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drainAndCount consumes events from sub for at most window, returning
// the count and the timestamps of each receive (relative to start).
// Used by the debounce tests to assert the timing shape of fan-outs.
func drainAndCount(sub <-chan struct{}, window time.Duration) (int, []time.Duration) {
	deadline := time.After(window)
	start := time.Now()
	var times []time.Duration
	count := 0
	for {
		select {
		case <-sub:
			count++
			times = append(times, time.Since(start))
		case <-deadline:
			return count, times
		}
	}
}

// newTestBroadcaster builds a broadcaster with debounce timings
// shrunk for fast tests: 20ms quiet, 100ms max. Same algebra
// as the production 100ms / 1s pair, just one-fifth the wall-clock.
func newTestBroadcaster() *broadcaster {
	b := newBroadcaster()
	b.minQuiet = 20 * time.Millisecond
	b.maxWait = 100 * time.Millisecond
	return b
}

// TestBroadcaster_SingleFiresAfterQuiet — the simplest debounce
// case: one Publish, one subscriber. The event should land exactly
// once, ~minQuiet after the Publish.
func TestBroadcaster_SingleFiresAfterQuiet(t *testing.T) {
	b := newTestBroadcaster()
	sub, cancel := b.Subscribe()
	defer cancel()

	start := time.Now()
	b.Publish()
	select {
	case <-sub:
		elapsed := time.Since(start)
		require.GreaterOrEqual(t, elapsed, b.minQuiet,
			"event fired before minQuiet — debounce should hold the first event for at least minQuiet")
		require.Less(t, elapsed, b.maxWait,
			"single-publish event waited past maxWait — shouldn't happen, quiet should win")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("event never arrived for a single Publish")
	}

	// No second event — the broadcaster should be quiescent now.
	select {
	case <-sub:
		t.Fatal("a second event arrived without a second Publish")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestBroadcaster_BurstCoalesces — many Publishes within the quiet
// window should fold into a single fan-out. The whole point of the
// debounce: a turn's hook-callback burst becomes one snapshot
// re-fetch, not one per write.
func TestBroadcaster_BurstCoalesces(t *testing.T) {
	b := newTestBroadcaster()
	sub, cancel := b.Subscribe()
	defer cancel()

	// 10 Publishes in rapid succession, each shorter than minQuiet
	// apart. The combined elapsed (90ms) is still well under maxWait
	// (100ms), so quiet should win — fire once after the last
	// publish + minQuiet.
	for range 10 {
		b.Publish()
		time.Sleep(time.Millisecond)
	}

	count, _ := drainAndCount(sub, b.minQuiet+b.maxWait+50*time.Millisecond)
	require.Equal(t, 1, count,
		"a burst of 10 Publishes within the quiet window must fan out exactly once")
}

// TestBroadcaster_SustainedHitsMaxWait — Publishes faster than
// minQuiet, sustained past maxWait, must fire at the max-wait
// boundary. Without this the broadcaster would starve dashboards
// indefinitely during heavy churn.
func TestBroadcaster_SustainedHitsMaxWait(t *testing.T) {
	b := newTestBroadcaster()
	sub, cancel := b.Subscribe()
	defer cancel()

	// Run the publisher in the background for slightly more than
	// maxWait, then stop. The publisher Publish()es every
	// minQuiet/2 (10ms), so the quiet window never settles during
	// the run — only maxWait can fire.
	stopPub := make(chan struct{})
	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		t := time.NewTicker(b.minQuiet / 2)
		defer t.Stop()
		for {
			select {
			case <-stopPub:
				return
			case <-t.C:
				b.Publish()
			}
		}
	}()

	// Run for ~3 maxWait windows so we observe steady-state
	// behaviour (events fire on the maxWait cadence, not just the
	// first one).
	runDur := 3 * b.maxWait
	start := time.Now()
	count, times := drainAndCount(sub, runDur)
	close(stopPub)
	<-pubDone
	t.Logf("sustained: %d events in %v at offsets %v", count, time.Since(start), times)

	require.GreaterOrEqual(t, count, 2,
		"sustained Publishes must keep firing at the maxWait boundary, not stall")
	// First event should land at approximately maxWait (not earlier — the
	// stream keeps re-arming the quiet deadline).
	require.GreaterOrEqual(t, times[0], b.maxWait-5*time.Millisecond,
		"first event under sustained churn should fire at the maxWait boundary, not before")
}

// TestBroadcaster_NoPublishMeansNoFire — purely defensive: a fresh
// broadcaster with no Publish call must never tick. Catches a
// regression where the timer would auto-arm.
func TestBroadcaster_NoPublishMeansNoFire(t *testing.T) {
	b := newTestBroadcaster()
	sub, cancel := b.Subscribe()
	defer cancel()

	select {
	case <-sub:
		t.Fatal("broadcaster fired without any Publish call")
	case <-time.After(b.maxWait + 50*time.Millisecond):
	}
}

// TestBroadcaster_CancelRemovesSubscriber — after cancel(), the
// subscriber must stop receiving events. Memory-leak / dangling-
// reference regression guard.
func TestBroadcaster_CancelRemovesSubscriber(t *testing.T) {
	b := newTestBroadcaster()
	sub, cancel := b.Subscribe()

	b.Publish()
	select {
	case <-sub:
	case <-time.After(b.maxWait + 50*time.Millisecond):
		t.Fatal("first event before cancel never arrived")
	}

	cancel()

	// Drain anything still buffered (should be empty), then a
	// post-cancel Publish must not be delivered.
	select {
	case <-sub:
	default:
	}
	b.Publish()
	select {
	case <-sub:
		t.Fatal("a cancelled subscriber still received an event")
	case <-time.After(b.maxWait + 50*time.Millisecond):
	}
}

// TestBroadcaster_ConcurrentPublishesSafe — sanity check that
// many goroutines hammering Publish in parallel don't trip the
// internal mutex / timer logic. Outcome only needs to be: no panic,
// at least one fan-out.
func TestBroadcaster_ConcurrentPublishesSafe(t *testing.T) {
	b := newTestBroadcaster()
	sub, cancel := b.Subscribe()
	defer cancel()

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 50 {
				b.Publish()
			}
		})
	}
	wg.Wait()

	count, _ := drainAndCount(sub, 3*b.maxWait)
	require.GreaterOrEqual(t, count, 1,
		"concurrent Publish storm should fan out at least once")
}

// TestPublishOnSuccessfulWrite — the mux-level middleware that
// turns every successful non-GET into a Publish. Covers all four
// cells of the (method × status) matrix:
//
//   - POST 200 → fires
//   - DELETE 204 → fires
//   - POST 400 → does NOT fire (bad request)
//   - GET 200 → does NOT fire (read-only)
//
// Without this the dashboard-mutation route family would drift
// silently as new handlers ship; the wrap is the contract.
func TestPublishOnSuccessfulWrite(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		respCode int
		want     bool
	}{
		{"POST 200 fires", http.MethodPost, http.StatusOK, true},
		{"POST 204 fires", http.MethodPost, http.StatusNoContent, true},
		{"DELETE 204 fires", http.MethodDelete, http.StatusNoContent, true},
		{"PATCH 200 fires", http.MethodPatch, http.StatusOK, true},
		{"POST 400 does not fire", http.MethodPost, http.StatusBadRequest, false},
		{"POST 500 does not fire", http.MethodPost, http.StatusInternalServerError, false},
		{"GET 200 does not fire", http.MethodGet, http.StatusOK, false},
		{"HEAD 200 does not fire", http.MethodHead, http.StatusOK, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Use a fresh broadcaster per test so timing from
			// earlier cases can't leak across.
			prev := dashboardEvents
			b := newTestBroadcaster()
			dashboardEvents = b
			t.Cleanup(func() { dashboardEvents = prev })

			sub, cancel := b.Subscribe()
			defer cancel()

			h := publishOnSuccessfulWrite(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.respCode)
			}))
			req := httptest.NewRequest(tc.method, "/api/anything", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, tc.respCode, rec.Code, "test handler must reach the recorder")

			select {
			case <-sub:
				if !tc.want {
					t.Fatal("event fired when it shouldn't have")
				}
			case <-time.After(b.maxWait + 50*time.Millisecond):
				if tc.want {
					t.Fatal("event never fired when it should have")
				}
			}
		})
	}
}

// TestPublishOnSuccessfulWrite_PreservesFlusher — the SSE handler
// relies on its response writer satisfying http.Flusher. The
// middleware skips the wrap entirely on GET, so a GET handler's
// `w.(http.Flusher)` must still succeed. This pins it.
func TestPublishOnSuccessfulWrite_PreservesFlusher(t *testing.T) {
	var sawFlusher bool
	h := publishOnSuccessfulWrite(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.True(t, sawFlusher,
		"GET must reach the underlying handler unwrapped so SSE can Flusher.Flush()")
}
