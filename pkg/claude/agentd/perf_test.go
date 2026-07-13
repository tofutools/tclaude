package agentd

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPerfRing_WrapsOldestFirst(t *testing.T) {
	r := &perfRing{}
	for i := range perfRingCap + 5 {
		r.add(perfSample{TotalMs: float64(i)})
	}
	got := r.ordered()
	require.Len(t, got, perfRingCap)
	// The 5 oldest samples were overwritten; order is oldest→newest.
	assert.Equal(t, float64(5), got[0].TotalMs)
	assert.Equal(t, float64(perfRingCap+4), got[len(got)-1].TotalMs)
}

func TestPerfRing_OrderedBeforeFull(t *testing.T) {
	r := &perfRing{}
	for i := range 3 {
		r.add(perfSample{TotalMs: float64(i)})
	}
	got := r.ordered()
	require.Len(t, got, 3)
	assert.Equal(t, float64(0), got[0].TotalMs)
	assert.Equal(t, float64(2), got[2].TotalMs)
}

func TestQuantilesOf(t *testing.T) {
	values := make([]float64, 0, 100)
	// Insert in descending order to prove sorting happens inside.
	for i := 100; i >= 1; i-- {
		values = append(values, float64(i))
	}
	q := quantilesOf(values)
	assert.Equal(t, 100, q.Count)
	assert.Equal(t, float64(50), q.P50Ms)
	assert.Equal(t, float64(90), q.P90Ms)
	assert.Equal(t, float64(99), q.P99Ms)
	assert.Equal(t, float64(100), q.MaxMs)

	empty := quantilesOf(nil)
	assert.Equal(t, 0, empty.Count)
	assert.Equal(t, float64(0), empty.MaxMs)

	one := quantilesOf([]float64{7})
	assert.Equal(t, float64(7), one.P50Ms)
	assert.Equal(t, float64(7), one.P99Ms)
}

func TestPerfSpan_NilSafe(t *testing.T) {
	var s *perfSpan
	assert.NotPanics(t, func() {
		s.mark("anything")
		s.addDuration("nested", time.Second)
	})
}

func TestWithPerfTiming_RecordsTotalAndPhases(t *testing.T) {
	ResetPerfForTest()
	t.Cleanup(ResetPerfForTest)

	h := withPerfTiming("/api/test", func(w http.ResponseWriter, r *http.Request) {
		span := perfSpanFrom(r)
		require.NotNil(t, span, "wrapped handler must find its span in the context")
		span.mark("alpha")
		span.mark("beta")
		w.WriteHeader(http.StatusOK)
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/test", nil))

	perfMu.Lock()
	ring := perfRings["/api/test"]
	perfMu.Unlock()
	require.NotNil(t, ring)
	samples := ring.ordered()
	require.Len(t, samples, 1)
	s := samples[0]
	assert.False(t, s.At.IsZero())
	// alpha + beta from the handler, plus the synthetic trailing "write"
	// phase covering the remainder after the last handler mark.
	require.Len(t, s.Phases, 3)
	assert.Equal(t, "alpha", s.Phases[0].Name)
	assert.Equal(t, "beta", s.Phases[1].Name)
	assert.Equal(t, "write", s.Phases[2].Name)
	// The total covers the whole request, so it can't be smaller than
	// the sum of its phases (they partition the same interval).
	var phaseMicros int64
	for _, p := range s.Phases {
		// durMs records whole microseconds as fractional milliseconds.
		// Compare at that source precision so summing float64 values cannot
		// turn (for example) 0.013 into 0.013000000000000001.
		phaseMicros += int64(math.Round(p.Ms * 1000))
	}
	totalMicros := int64(math.Round(s.TotalMs * 1000))
	assert.GreaterOrEqual(t, totalMicros, phaseMicros)
}

func TestWithPerfTiming_NoPhasesMeansNoSyntheticWrite(t *testing.T) {
	ResetPerfForTest()
	t.Cleanup(ResetPerfForTest)

	h := withPerfTiming("/api/plain", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/plain", nil))

	perfMu.Lock()
	ring := perfRings["/api/plain"]
	perfMu.Unlock()
	require.NotNil(t, ring)
	samples := ring.ordered()
	require.Len(t, samples, 1)
	assert.Empty(t, samples[0].Phases)
}

func TestPerfEndpointViewOf_AggregatesAndLimit(t *testing.T) {
	at := time.Now()
	samples := []perfSample{
		{At: at, TotalMs: 10, Phases: []perfPhase{{Name: "a", Ms: 4}, {Name: "b", Ms: 6}}},
		{At: at, TotalMs: 20, Phases: []perfPhase{{Name: "a", Ms: 8}, {Name: "b", Ms: 12}}},
		{At: at, TotalMs: 30, Phases: []perfPhase{{Name: "a", Ms: 12}, {Name: "b", Ms: 18}}},
	}
	v := perfEndpointViewOf("/api/x", samples, 2)
	assert.Equal(t, "/api/x", v.Endpoint)
	// Aggregates cover all 3 samples even though only 2 raw samples ship.
	assert.Equal(t, 3, v.Count)
	assert.Equal(t, float64(30), v.MaxMs)
	require.Len(t, v.Samples, 2)
	assert.Equal(t, float64(20), v.Samples[0].TotalMs)
	assert.Equal(t, float64(30), v.Samples[1].TotalMs)
	require.Len(t, v.Phases, 2)
	assert.Equal(t, "a", v.Phases[0].Name)
	assert.Equal(t, 3, v.Phases[0].Count)
	assert.Equal(t, float64(12), v.Phases[0].MaxMs)
	assert.Equal(t, "b", v.Phases[1].Name)
	assert.Equal(t, float64(18), v.Phases[1].MaxMs)
}

func BenchmarkPerfPayload(b *testing.B) {
	for _, sampleCount := range []int{1, perfRingCap} {
		b.Run(fmt.Sprintf("samples_%d", sampleCount), func(b *testing.B) {
			samplesByEndpoint := benchmarkPerfSamples(sampleCount)
			views := make([]perfEndpointView, 0, len(samplesByEndpoint))
			for endpoint, samples := range samplesByEndpoint {
				views = append(views, perfEndpointViewOf(endpoint, samples, 240))
			}
			envelope := struct {
				GeneratedAt string             `json:"generated_at"`
				Endpoints   []perfEndpointView `json:"endpoints"`
			}{GeneratedAt: "2026-07-12T00:00:00Z", Endpoints: views}
			payload, err := json.Marshal(envelope)
			require.NoError(b, err)
			var compressed bytes.Buffer
			zw := gzip.NewWriter(&compressed)
			_, err = zw.Write(payload)
			require.NoError(b, err)
			require.NoError(b, zw.Close())

			b.Run("aggregate", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for endpoint, samples := range samplesByEndpoint {
						_ = perfEndpointViewOf(endpoint, samples, 240)
					}
				}
			})
			b.Run("marshal", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, err := json.Marshal(envelope)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(payload)), "payload_bytes")
				b.ReportMetric(float64(compressed.Len()), "gzip_bytes")
			})
			b.Run("gzip", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var dst bytes.Buffer
					zw := gzip.NewWriter(&dst)
					if _, err := zw.Write(payload); err != nil {
						b.Fatal(err)
					}
					if err := zw.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func benchmarkPerfSamples(sampleCount int) map[string][]perfSample {
	endpoints := map[string]int{
		"/api/conversations": 5,
		"/api/jobs":          5,
		"/api/replaced":      5,
		"/api/retired":       5,
		"/api/snapshot":      8,
	}
	out := make(map[string][]perfSample, len(endpoints))
	for endpoint, phaseCount := range endpoints {
		samples := make([]perfSample, sampleCount)
		for i := range samples {
			phases := make([]perfPhase, phaseCount)
			for j := range phases {
				phases[j] = perfPhase{Name: fmt.Sprintf("phase_%d", j), Ms: float64((i+j)%100) / 10}
			}
			samples[i] = perfSample{
				At:      time.Unix(int64(i), 0).UTC(),
				TotalMs: float64(i%200) / 10,
				Phases:  phases,
			}
		}
		out[endpoint] = samples
	}
	return out
}
