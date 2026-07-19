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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type usageHistoryResp struct {
	From   string `json:"from"`
	Series []struct {
		Provider   string `json:"provider"`
		WindowName string `json:"window_name"`
		From       string `json:"from"`
		Points     []struct {
			At       string  `json:"at"`
			Pct      float64 `json:"pct"`
			Excluded bool    `json:"excluded"`
		} `json:"points"`
		Resets []struct {
			Pct float64 `json:"pct"`
		} `json:"resets"`
		Forecast struct {
			Status      string  `json:"status"`
			BaselinePct float64 `json:"baseline_pct"`
		} `json:"forecast"`
	} `json:"series"`
}

func TestDashboardUsageHistoryExcludesAndRestoresPoint(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(15 * time.Minute)
	for i, pct := range []float64{10, 90, 20, 30} {
		_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderOpenAI, ObservedAt: base.Add(time.Duration(i) * 15 * time.Minute),
			Windows: []db.SubscriptionUsageWindow{{
				Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: pct, ResetsAt: base.Add(6 * time.Hour),
			}},
		})
		require.NoError(t, err)
	}
	mux := agentd.BuildDashboardHandlerForTest()
	pointAt := base.Add(15 * time.Minute).Format(time.RFC3339Nano)
	setExcluded := func(excluded bool) *httptest.ResponseRecorder {
		return testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
			"/api/usage-history/point", map[string]any{
				"provider": "openai", "window_name": "five_hour", "at": pointAt, "excluded": excluded,
			}))
	}

	rec := setExcluded(true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=24", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out usageHistoryResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Series, 1)
	require.Len(t, out.Series[0].Points, 4, "excluded observation remains reversible chart data")
	assert.True(t, out.Series[0].Points[1].Excluded)
	assert.Empty(t, out.Series[0].Resets, "excluded spike cannot manufacture a reset")
	assert.Equal(t, 10.0, out.Series[0].Forecast.BaselinePct)
	assert.Equal(t, "before_reset", out.Series[0].Forecast.Status)

	rec = setExcluded(false)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=24", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out = usageHistoryResp{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.False(t, out.Series[0].Points[1].Excluded)
	require.Len(t, out.Series[0].Resets, 1, "restored spike participates in reset detection again")
	assert.Equal(t, 20.0, out.Series[0].Forecast.BaselinePct)
}

func TestDashboardUsageHistorySeriesForecastAndVisibility(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(15 * time.Minute)
	for i, pct := range []float64{80, 20, 25, 30} {
		_, err := db.SaveCodexUsageCacheIfNewer(json.RawMessage(`{}`), base.Add(time.Duration(i)*15*time.Minute), "rollout",
			db.SubscriptionUsageWindow{Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: pct, ResetsAt: base.Add(6 * time.Hour)})
		require.NoError(t, err)
	}
	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=24", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out usageHistoryResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Series, 1)
	assert.Equal(t, "openai", out.Series[0].Provider)
	assert.Equal(t, "five_hour", out.Series[0].WindowName)
	assert.Len(t, out.Series[0].Points, 4)
	require.Len(t, out.Series[0].Resets, 1)
	assert.Equal(t, 20.0, out.Series[0].Resets[0].Pct)
	assert.Equal(t, 20.0, out.Series[0].Forecast.BaselinePct)
	assert.Equal(t, "before_reset", out.Series[0].Forecast.Status)

	snapshot := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snapshot.Code, snapshot.Body.String())
	var snap struct {
		UsageTabVisible bool `json:"usage_tab_visible"`
	}
	require.NoError(t, json.Unmarshal(snapshot.Body.Bytes(), &snap))
	assert.True(t, snap.UsageTabVisible, "a retained subscription cache keeps the Usage tab visible")
}

func TestDashboardUsageHistoryPerSeriesSpans(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	now := time.Now().UTC().Truncate(15 * time.Minute)
	for i, pct := range []float64{10, 15, 20} {
		_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderAnthropic, ObservedAt: now.Add(-72*time.Hour + time.Duration(i)*15*time.Minute),
			Windows: []db.SubscriptionUsageWindow{{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: pct}},
		})
		require.NoError(t, err)
	}
	for i, pct := range []float64{30, 35} {
		_, err := db.SaveCodexUsageCacheIfNewer(json.RawMessage(`{}`), now.Add(-time.Hour+time.Duration(i)*15*time.Minute), "rollout",
			db.SubscriptionUsageWindow{Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: pct, ResetsAt: now.Add(4 * time.Hour)})
		require.NoError(t, err)
	}
	mux := agentd.BuildDashboardHandlerForTest()

	byKey := func(out usageHistoryResp) map[string]int {
		index := map[string]int{}
		for i, series := range out.Series {
			index[series.Provider+":"+series.WindowName] = i
		}
		return index
	}

	// Without an override the 24h default clips the 3-day-old Claude samples
	// away, but the series stays in the response so its card keeps rendering.
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=24", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out usageHistoryResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Series, 2)
	index := byKey(out)
	assert.Empty(t, out.Series[index["anthropic:seven_day"]].Points, "stale series retained with empty points")
	assert.Len(t, out.Series[index["openai:five_hour"]].Points, 2)

	// A per-series override widens only the Claude series' view.
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet,
		"/api/usage-history?hours=24&spans=anthropic:seven_day:168", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out = usageHistoryResp{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Series, 2)
	index = byKey(out)
	claude := out.Series[index["anthropic:seven_day"]]
	assert.Len(t, claude.Points, 3, "override admits the 3-day-old samples")
	assert.NotEqual(t, out.From, claude.From, "overridden series reports its own view start")
	codex := out.Series[index["openai:five_hour"]]
	assert.Len(t, codex.Points, 2)
	assert.Equal(t, out.From, codex.From, "non-overridden series keeps the default view start")

	for _, bad := range []string{"nonsense", "a:b:0", "a:b:2161", "a:b:c:1", ":seven_day:24"} {
		rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=24&spans="+bad, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "spans=%s", bad)
	}
}

func TestDashboardUsageHistoryRejectsOversizedSpan(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=2161", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDashboardUsageTabStaysHiddenForCacheWithoutQuotaHistory(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	now := time.Now().UTC()
	require.NoError(t, db.SaveUsageCache(json.RawMessage(`{}`), now, now), "seed a cache row with no rolling windows")
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snap struct {
		UsageTabVisible bool `json:"usage_tab_visible"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	assert.False(t, snap.UsageTabVisible, "pay-per-token cache rows do not expose an empty Usage tab")
}

func TestDashboardUsageHistoryPausesExpiredForecast(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(15 * time.Minute)
	for i, pct := range []float64{10, 15, 20} {
		_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
			Provider: db.SubscriptionProviderAnthropic, ObservedAt: base.Add(time.Duration(i) * 15 * time.Minute),
			Windows: []db.SubscriptionUsageWindow{{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: pct}},
		})
		require.NoError(t, err)
	}
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodGet, "/api/usage-history?hours=24", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out usageHistoryResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Series, 1)
	assert.Equal(t, "stale", out.Series[0].Forecast.Status)
}
