package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
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
	wantPhases := []string{"tmux_ls", "preload", "groups", "roster", "assemble", "collectors", "write"}
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
	// The list handlers record totals only — no phase marks.
	assert.Empty(t, retired.Samples[0].Phases)

	// ?limit trims the raw samples served, never the aggregates.
	limited := fetchPerf(t, dash, "/api/perf?limit=1")
	snapLimited := perfEndpointNamed(t, limited, "/api/snapshot")
	assert.Equal(t, 2, snapLimited.Count)
	require.Len(t, snapLimited.Samples, 1)
}
