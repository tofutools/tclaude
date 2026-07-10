package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Functional tests for /api/cost-factor — the persistence backend of the
// Costs tab's live display multiplier (costs.js) and the twin of the
// Config tab's cost.estimate_factor field. Same harness as the slop /
// config tests: setupTestDB points HOME at a temp dir so
// config.ConfigPath() is isolated, withDashboardAuth satisfies the
// cookie gate.

// costFactorResp is the GET / POST response shape.
type costFactorResp struct {
	EstimateFactor *float64 `json:"estimate_factor"`
	Error          string   `json:"error"`
	Code           string   `json:"code"`
}

func serveCostFactor(t *testing.T, method, body string) (*httptest.ResponseRecorder, costFactorResp) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cost-factor", handleDashboardCostFactorAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(method, "/api/cost-factor", body))
	var resp costFactorResp
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

func TestDashboardCostFactor_GetDefault(t *testing.T) {
	setupTestDB(t) // HOME → temp dir, no config file yet
	withDashboardAuth(t)

	w, resp := serveCostFactor(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, resp.EstimateFactor)
	assert.Equal(t, 1.0, *resp.EstimateFactor, "absent config defaults to a no-op factor")
}

func TestDashboardCostFactor_PostPersistsAndMerges(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Seed an unrelated setting to prove the factor write merges into the
	// existing config rather than replacing it.
	require.NoError(t, config.Save(&config.Config{LogLevel: "debug"}))

	w, resp := serveCostFactor(t, http.MethodPost, `{"estimate_factor":1.1}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, resp.EstimateFactor)
	assert.InDelta(t, 1.1, *resp.EstimateFactor, 1e-9)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel, "unrelated settings must survive a factor write")
	assert.InDelta(t, 1.1, cfg.ResolvedCostFactor(), 1e-9)
}

// Posting 1 (or null) is the no-op default — the override is cleared and
// the now-empty cost block dropped, so config.json never accrues a
// redundant "cost": { "estimate_factor": 1 }.
func TestDashboardCostFactor_PostOneClearsOverride(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Set a real factor, then clear it two ways.
	_, _ = serveCostFactor(t, http.MethodPost, `{"estimate_factor":1.25}`)
	require.InDelta(t, 1.25, mustLoadFactor(t), 1e-9)

	w, resp := serveCostFactor(t, http.MethodPost, `{"estimate_factor":1}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, resp.EstimateFactor)
	assert.Equal(t, 1.0, *resp.EstimateFactor)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.Cost, "factor 1 clears the whole cost block")

	// And again via explicit null after re-setting.
	_, _ = serveCostFactor(t, http.MethodPost, `{"estimate_factor":1.25}`)
	require.InDelta(t, 1.25, mustLoadFactor(t), 1e-9)
	w, _ = serveCostFactor(t, http.MethodPost, `{"estimate_factor":null}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.Cost, "null clears the override")
}

func TestDashboardCostFactor_PostRejectsBadInput(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	for name, body := range map[string]string{
		"zero":       `{"estimate_factor":0}`,
		"negative":   `{"estimate_factor":-1}`,
		"over range": `{"estimate_factor":11}`,
		"not json":   `nope`,
	} {
		w, _ := serveCostFactor(t, http.MethodPost, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s must 400; body=%s", name, w.Body.String())
	}
	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "rejected posts must not create the config file")
}

// A hand-edited out-of-range factor parses fine; Validate flags it in
// the Config tab, but GET must still hand the browser a usable clamped
// value rather than echoing back something the next POST would reject.
func TestDashboardCostFactor_GetClampsHandEdited(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(),
		[]byte(`{"cost":{"estimate_factor":1000}}`), 0o644))

	w, resp := serveCostFactor(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, resp.EstimateFactor)
	assert.Equal(t, 10.0, *resp.EstimateFactor, "over-range clamps to the max")
}

func TestDashboardCostFactor_PostRefusesCorruptConfig(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte("{not json"), 0o644))

	w, resp := serveCostFactor(t, http.MethodPost, `{"estimate_factor":1.1}`)
	assert.Equal(t, http.StatusConflict, w.Code, "a corrupt config must not be silently replaced")
	assert.Equal(t, "malformed_target", resp.Code)

	data, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, "{not json", string(data), "the corrupt file must be left untouched")
}

func mustLoadFactor(t *testing.T) float64 {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg.ResolvedCostFactor()
}

// applyCostDisplayFactor scales every per-agent cost (Agents, Ungrouped,
// and each group member) plus the top-bar usage figures, and is a no-op
// at factor 1 so the common path is untouched.
func TestApplyCostDisplayFactor(t *testing.T) {
	build := func() snapshotPayload {
		return snapshotPayload{
			Usage:     dashboardUsage{TotalCostUSD: 10, TodayCostUSD: 2},
			Agents:    []dashboardAgent{{ConvID: "a", State: agentState{CostUSD: 4}}},
			Ungrouped: []dashboardAgent{{ConvID: "u", State: agentState{CostUSD: 1}}},
			Groups: []dashboardGroup{{
				Name:    "g",
				Members: []dashboardMember{{ConvID: "m", State: agentState{CostUSD: 8}}},
			}},
		}
	}

	noop := build()
	applyCostDisplayFactor(&noop, 1)
	assert.Equal(t, 10.0, noop.Usage.TotalCostUSD, "factor 1 leaves usage untouched")
	assert.Equal(t, 4.0, noop.Agents[0].State.CostUSD, "factor 1 leaves agents untouched")

	scaled := build()
	applyCostDisplayFactor(&scaled, 1.5)
	assert.InDelta(t, 15.0, scaled.Usage.TotalCostUSD, 1e-9)
	assert.InDelta(t, 3.0, scaled.Usage.TodayCostUSD, 1e-9)
	assert.InDelta(t, 6.0, scaled.Agents[0].State.CostUSD, 1e-9)
	assert.InDelta(t, 1.5, scaled.Ungrouped[0].State.CostUSD, 1e-9)
	assert.InDelta(t, 12.0, scaled.Groups[0].Members[0].State.CostUSD, 1e-9)
}
