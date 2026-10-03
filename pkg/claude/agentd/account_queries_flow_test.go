package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func accountQuery(t *testing.T, f *testharness.Flow, conv, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, path, nil)
	if conv == "" {
		r = agentd.AsHumanPeer(r)
	} else {
		r = agentd.AsAgentPeer(r, conv)
	}
	return testharness.Serve(f.Mux, r)
}

func TestAccountQueriesPermissionsAreSeparateAndDenyWins(t *testing.T) {
	f := newFlow(t)
	const conv = "quota-reader"
	f.HaveConvWithTitle(conv, "quota-reader")
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: []string{agentd.PermUsageRead}}}))
	rec := accountQuery(t, f, conv, "/v1/usage/summary")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "cost_usd")
	rec = accountQuery(t, f, conv, "/v1/costs")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/permissions/deny", map[string]any{"target": conv, "slug": agentd.PermUsageRead})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = accountQuery(t, f, conv, "/v1/usage/summary")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	for _, path := range []string{"/v1/usage/summary", "/v1/costs"} {
		rec = accountQuery(t, f, "", path)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = testharness.Serve(f.Mux, agentd.AsUnconfirmedPeer(testharness.JSONRequest(t, http.MethodGet, path, nil)))
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
}

func TestAccountUsagePreservesMissingExpiredAndStaleObservations(t *testing.T) {
	f := newFlow(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedUsageCache(t, usageapi.CachedUsage{FiveHour: &usageapi.CachedBucket{Pct: 72, ResetsAt: now.Add(-time.Minute)}, SevenDaySonnet: &usageapi.CachedBucket{Pct: 15, ResetsAt: now.Add(24 * time.Hour)}, FetchedAt: now})
	for i, pct := range []float64{10, 20, 30} {
		_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{Provider: db.SubscriptionProviderOpenAI, ObservedAt: now.Add(time.Duration(i-2) * 30 * time.Minute), Windows: []db.SubscriptionUsageWindow{{Name: "seven_day", UsedPercent: pct, ResetsAt: now.Add(7 * 24 * time.Hour)}}})
		require.NoError(t, err)
	}
	_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{Provider: "old-provider", ObservedAt: now.Add(-4 * 24 * time.Hour), Windows: []db.SubscriptionUsageWindow{{Name: "monthly", UsedPercent: 55, ResetsAt: now.Add(24 * time.Hour)}}})
	require.NoError(t, err)
	rec := accountQuery(t, f, "", "/v1/usage/summary")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Windows []struct {
			Provider  string  `json:"provider"`
			Name      string  `json:"window_name"`
			Available bool    `json:"available"`
			Status    string  `json:"status"`
			Pct       float64 `json:"pct"`
			Observed  string  `json:"observed_at"`
			Forecasts map[string]struct {
				Status string  `json:"status"`
				Rate   float64 `json:"rate_pct_per_hour"`
			} `json:"forecasts"`
		} `json:"windows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Windows, 4, "missing Claude weekly window must not be synthesized")
	for _, w := range out.Windows {
		if w.Provider != "anthropic" {
			assert.NotEmpty(t, w.Observed)
		}
		switch w.Provider + ":" + w.Name {
		case "anthropic:five_hour":
			assert.Equal(t, "reset", w.Status)
			assert.False(t, w.Available)
			assert.Equal(t, 72.0, w.Pct)
		case "anthropic:seven_day_sonnet":
			assert.False(t, w.Available)
			assert.Equal(t, "unknown_age", w.Status)
			assert.Empty(t, w.Observed)
			assert.Equal(t, 15.0, w.Pct)
		case "openai:seven_day":
			assert.True(t, w.Available)
			assert.Equal(t, 30.0, w.Pct)
			assert.Equal(t, "before_reset", w.Forecasts["span"].Status)
			assert.Greater(t, w.Forecasts["span"].Rate, 0.0)
		case "old-provider:monthly":
			assert.Equal(t, "stale", w.Status)
			assert.False(t, w.Available)
			assert.Equal(t, 55.0, w.Pct)
		default:
			t.Fatalf("unexpected window: %+v", w)
		}
	}
}

func TestAccountCostsRawSplitAndSelfIncludesPriorGenerations(t *testing.T) {
	f := newFlow(t)
	const conv = "spending-agent"
	f.HaveConvWithTitle(conv, "spending-agent")
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	require.NoError(t, db.LinkConvToAgent("prior-generation", agentID, "prior", "test"))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: []string{agentd.PermCostsRead}}, Cost: &config.CostConfig{ShowOnSubscription: true, HarnessFactors: map[string]float64{"claude": 9}}}))
	for _, row := range []struct {
		id, conv      string
		real, virtual float64
	}{{"cost-old", "prior-generation", 2, 0}, {"cost-current", conv, 0, 3}, {"cost-other", "other-agent", 7, 0}} {
		require.NoError(t, db.SaveSession(&db.SessionRow{ID: row.id, TmuxSession: row.id, ConvID: row.conv, Cwd: "/tmp", Status: "idle", Harness: "claude"}))
		if row.real > 0 {
			require.NoError(t, db.UpdateSessionCost(row.id, row.real))
		} else {
			require.NoError(t, db.UpdateSessionVirtualCost(row.id, row.virtual))
		}
	}
	rec := accountQuery(t, f, conv, "/v1/costs?self=true")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		costsResp
		Scope       string  `json:"scope"`
		AgentID     string  `json:"agent_id"`
		TodayReal   float64 `json:"today_real_usd"`
		TodayWhatIf float64 `json:"today_what_if_usd"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "self", out.Scope)
	assert.Equal(t, agentID, out.AgentID)
	assert.Equal(t, 2.0, out.RealTotalUSD)
	assert.Equal(t, 3.0, out.WhatIfTotalUSD)
	assert.Equal(t, 5.0, out.TotalUSD)
	assert.Equal(t, 2.0, out.TodayReal)
	assert.Equal(t, 3.0, out.TodayWhatIf)
	require.Len(t, out.Agents, 2)
	assert.NotContains(t, rec.Body.String(), "other-agent")
	rec = accountQuery(t, f, conv, "/v1/costs")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 9.0, out.RealTotalUSD)
	rec = accountQuery(t, f, "", "/v1/costs?self=true")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountCostsRejectsBadRanges(t *testing.T) {
	f := newFlow(t)
	for _, q := range []string{"from=bad", "to=bad", "from=2026-04-02&to=2026-04-01", "from=2020-01-01&to=2026-01-01", "self=false"} {
		rec := accountQuery(t, f, "", "/v1/costs?"+q)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestAccountUsageClaudeCarryForwardDoesNotRefreshWindowAge(t *testing.T) {
	f := newFlow(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-4 * 24 * time.Hour)
	reset := now.Add(2 * 24 * time.Hour)
	_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
		Provider: db.SubscriptionProviderAnthropic, ObservedAt: old, Source: "statusline",
		Windows: []db.SubscriptionUsageWindow{{Name: "seven_day", UsedPercent: 40, ResetsAt: reset}},
	})
	require.NoError(t, err)
	seedUsageCache(t, usageapi.CachedUsage{SevenDay: &usageapi.CachedBucket{Pct: 40, ResetsAt: reset}, FetchedAt: old})
	// Production statusline update omits weekly: the cache carries it forward,
	// while only five_hour receives a new per-window history observation.
	usageapi.UpdateFromStatusLine(&usageapi.CachedBucket{Pct: 10, ResetsAt: now.Add(time.Hour)}, nil, nil)
	rec := accountQuery(t, f, "", "/v1/usage/summary")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Windows []struct {
			Name       string `json:"window_name"`
			Status     string `json:"status"`
			ObservedAt string `json:"observed_at"`
			Age        int64  `json:"age_seconds"`
			Available  bool   `json:"available"`
		} `json:"windows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Windows, 2)
	for _, w := range out.Windows {
		if w.Name == "seven_day" {
			assert.Equal(t, "stale", w.Status)
			assert.False(t, w.Available)
			assert.Equal(t, old.Format(time.RFC3339Nano), w.ObservedAt)
			assert.GreaterOrEqual(t, w.Age, int64(4*24*60*60))
		} else {
			assert.Equal(t, "current", w.Status)
			assert.True(t, w.Available)
			assert.Less(t, w.Age, int64(60))
		}
	}
}

func TestAccountCostsSelfRejectsUnlinkedConversation(t *testing.T) {
	f := newFlow(t)
	const conv = "unlinked-cost-reader"
	f.HaveConvWithTitle(conv, conv)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: []string{agentd.PermCostsRead}}}))
	rec := accountQuery(t, f, conv, "/v1/costs?self=true")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "agent_required")
}
