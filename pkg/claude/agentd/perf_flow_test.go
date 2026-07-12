package agentd_test

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// perfPayload mirrors the /api/perf envelope (agentd's unexported view
// structs), same convention as dashSnapshot.
type perfPayload struct {
	GeneratedAt string             `json:"generated_at"`
	Endpoints   []perfEndpointJSON `json:"endpoints"`
}

type perfEndpointJSON struct {
	Endpoint string           `json:"endpoint"`
	Count    int              `json:"count"`
	P50Ms    float64          `json:"p50_ms"`
	P99Ms    float64          `json:"p99_ms"`
	MaxMs    float64          `json:"max_ms"`
	Phases   []perfPhaseJSON  `json:"phases"`
	Samples  []perfSampleJSON `json:"samples"`
}

type perfPhaseJSON struct {
	Name  string  `json:"name"`
	Count int     `json:"count"`
	MaxMs float64 `json:"max_ms"`
}

type perfSampleJSON struct {
	At      string  `json:"at"`
	TotalMs float64 `json:"total_ms"`
	Phases  []struct {
		Name string  `json:"name"`
		Ms   float64 `json:"ms"`
	} `json:"phases"`
}

func fetchPerf(t *testing.T, mux http.Handler, path string) perfPayload {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, rec.Code, "%s body=%s", path, rec.Body.String())
	var out perfPayload
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode perf payload")
	return out
}

func perfEndpointNamed(t *testing.T, p perfPayload, name string) perfEndpointJSON {
	t.Helper()
	for _, e := range p.Endpoints {
		if e.Endpoint == name {
			return e
		}
	}
	require.Failf(t, "endpoint missing from /api/perf", "want %s, have %v", name, p.Endpoints)
	return perfEndpointJSON{}
}

// TestDashboardPerf_RecordsPollTimings drives the real dashboard poll
// surface — /api/snapshot twice, /api/retired once — and asserts
// /api/perf reports both endpoints with per-request samples, plus the
// snapshot's named phase breakdown ending in the synthetic write phase.
func TestDashboardPerf_RecordsPollTimings(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	agentd.ResetPerfForTest()
	t.Cleanup(agentd.ResetPerfForTest)
	dash := agentd.BuildDashboardHandlerForTest()

	// An empty recorder serves an empty endpoint list, not an error.
	empty := fetchPerf(t, dash, "/api/perf")
	assert.Empty(t, empty.Endpoints)

	fetchSnapshotOnly(t, dash)
	fetchSnapshotOnly(t, dash)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/retired?limit=0", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	perf := fetchPerf(t, dash, "/api/perf")
	require.NotEmpty(t, perf.GeneratedAt)

	snap := perfEndpointNamed(t, perf, "/api/snapshot")
	assert.Equal(t, 2, snap.Count)
	require.Len(t, snap.Samples, 2)
	// The handler's named phases arrive in execution order, with the
	// wrapper's synthetic "write" (JSON encode + socket write) last.
	wantPhases := []string{"tmux_ls", "preload", "groups", "codex_telemetry", "roster", "assemble", "collectors", "write"}
	var gotPhases []string
	for _, p := range snap.Samples[0].Phases {
		gotPhases = append(gotPhases, p.Name)
	}
	assert.Equal(t, wantPhases, gotPhases)
	// Aggregates carry one row per phase, covering both samples.
	require.Len(t, snap.Phases, len(wantPhases))
	assert.Equal(t, 2, snap.Phases[0].Count)

	retired := perfEndpointNamed(t, perf, "/api/retired")
	assert.Equal(t, 1, retired.Count)
	require.Len(t, retired.Samples, 1)
	// /api/retired now records its named phase breakdown (TCL-368), in
	// execution order, with the wrapper's synthetic "write" last — so the
	// operator's Debug tab can see where the retired page's time goes just like
	// the snapshot's.
	wantRetiredPhases := []string{"count", "page", "tmux_ls", "rows", "write"}
	var gotRetiredPhases []string
	for _, p := range retired.Samples[0].Phases {
		gotRetiredPhases = append(gotRetiredPhases, p.Name)
	}
	assert.Equal(t, wantRetiredPhases, gotRetiredPhases)

	// ?limit trims the raw samples served, never the aggregates.
	limited := fetchPerf(t, dash, "/api/perf?limit=1")
	snapLimited := perfEndpointNamed(t, limited, "/api/snapshot")
	assert.Equal(t, 2, snapLimited.Count)
	require.Len(t, snapLimited.Samples, 1)
}

func TestDashboardPerf_Gzip(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	agentd.ResetPerfForTest()
	t.Cleanup(agentd.ResetPerfForTest)
	dash := agentd.BuildDashboardHandlerForTest()

	fetchSnapshotOnly(t, dash)
	req := testharness.JSONRequest(t, http.MethodGet, "/api/perf", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := testharness.Serve(dash, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	assert.Contains(t, rec.Header().Values("Vary"), "Accept-Encoding")

	zr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	decoded, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	var payload perfPayload
	require.NoError(t, json.Unmarshal(decoded, &payload))
	assert.Equal(t, 1, perfEndpointNamed(t, payload, "/api/snapshot").Count)
}

// TestDashboardSnapshot_DebugTabVisibilityRule pins the Debug tab's
// auto-hide flag the front-end keys off (TCL-376): hidden by default,
// shown only by the explicit config dashboard.show_debug_tab opt-in.
// The gate is display-only — /api/perf keeps recording + serving either
// way, which is also asserted here so flipping the toggle can never
// silently disable the recorder.
func TestDashboardSnapshot_DebugTabVisibilityRule(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	agentd.ResetPerfForTest()
	t.Cleanup(agentd.ResetPerfForTest)
	dash := agentd.BuildDashboardHandlerForTest()

	// Default: no config → tab hidden.
	snap := fetchSnapshotOnly(t, dash)
	assert.False(t, snap.DebugTabVisible, "Debug tab hidden without the opt-in")

	// Opt in → visible on the next poll, no daemon restart.
	require.NoError(t, config.Save(&config.Config{
		Dashboard: &config.DashboardConfig{ShowDebugTab: true},
	}), "save config with show_debug_tab")
	snap = fetchSnapshotOnly(t, dash)
	assert.True(t, snap.DebugTabVisible, "dashboard.show_debug_tab shows the Debug tab")

	// The recorder ran regardless of the flag: both polls above landed in
	// the ring, and /api/perf serves them even when the tab is hidden.
	perf := fetchPerf(t, dash, "/api/perf")
	assert.Equal(t, 2, perfEndpointNamed(t, perf, "/api/snapshot").Count,
		"recording is not gated by the display toggle")
}

// TestDashboardPerf_Reset drives POST /api/perf/reset (TCL-377): a
// recorded distribution is discarded so a fresh one starts — the
// operator's "I just changed the setup under measurement" button —
// and recording resumes normally afterwards.
func TestDashboardPerf_Reset(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	agentd.ResetPerfForTest()
	t.Cleanup(agentd.ResetPerfForTest)
	dash := agentd.BuildDashboardHandlerForTest()

	fetchSnapshotOnly(t, dash)
	fetchSnapshotOnly(t, dash)
	require.Equal(t, 2, perfEndpointNamed(t, fetchPerf(t, dash, "/api/perf"), "/api/snapshot").Count,
		"two samples recorded before the reset")

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/perf/reset", nil))
	require.Equal(t, http.StatusOK, rec.Code, "reset body=%s", rec.Body.String())

	// Every ring is gone — the payload returns to its empty state...
	assert.Empty(t, fetchPerf(t, dash, "/api/perf").Endpoints, "reset discards every endpoint's ring")

	// ...and the next poll starts a fresh distribution (count restarts at 1;
	// the /api/perf GETs themselves are not a polled endpoint and record nothing).
	fetchSnapshotOnly(t, dash)
	assert.Equal(t, 1, perfEndpointNamed(t, fetchPerf(t, dash, "/api/perf"), "/api/snapshot").Count,
		"recording resumes fresh after the reset")

	// GET on the reset path is refused — the mux pattern is POST-only.
	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/perf/reset", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "GET /api/perf/reset must be refused")
}
