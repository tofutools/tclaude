package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func usageHistoryRows(base time.Time, values ...float64) []db.SubscriptionUsageHistoryRow {
	rows := make([]db.SubscriptionUsageHistoryRow, len(values))
	for i, value := range values {
		rows[i] = db.SubscriptionUsageHistoryRow{
			Provider: db.SubscriptionProviderOpenAI, WindowName: "five_hour",
			ObservedAt: base.Add(time.Duration(i) * 15 * time.Minute), UsedPercent: value,
		}
	}
	return rows
}

func TestForecastUsageDetectsUnexpectedNonzeroReset(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 80, 20, 25, 30)
	for i := range rows {
		rows[i].ResetsAt = base.Add(10 * time.Hour)
	}

	forecast, resets := forecastUsage(rows)
	require.Len(t, resets, 1)
	assert.Equal(t, 20.0, resets[0].Pct, "the observed post-reset minimum is retained; no synthetic zero")
	assert.Equal(t, "before_reset", forecast.Status)
	assert.Equal(t, 20.0, forecast.BaselinePct)
	assert.Equal(t, 3, forecast.SampleCount)
	assert.InDelta(t, 20.0, forecast.RatePctPerHour, 1e-9)
	assert.Equal(t, base.Add(4*time.Hour+15*time.Minute).Format(time.RFC3339Nano), forecast.HitsLimitAt)
}

func TestForecastUsageKnownBoundaryStartsNewSegmentWithoutDrop(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 10, 15, 20, 25)
	rows[0].ResetsAt = base.Add(15 * time.Minute)
	for i := 1; i < len(rows); i++ {
		rows[i].ResetsAt = base.Add(5 * time.Hour)
	}

	forecast, resets := forecastUsage(rows)
	require.Len(t, resets, 1, "a crossed declared boundary is recorded exactly once")
	assert.Equal(t, 15.0, forecast.BaselinePct)
	assert.Equal(t, 3, forecast.SampleCount)
	assert.Equal(t, "before_reset", forecast.Status)
}

func TestForecastUsageRequiresEnoughPostResetHistory(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	forecast, resets := forecastUsage(usageHistoryRows(base, 12, 13))
	assert.Empty(t, resets)
	assert.Equal(t, "insufficient", forecast.Status)
	assert.Zero(t, forecast.RatePctPerHour)
}

func TestForecastUsageReportsResetFirstAndFlatPaces(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	slow := usageHistoryRows(base, 10, 11, 12)
	for i := range slow {
		slow[i].ResetsAt = base.Add(time.Hour)
	}
	forecast, _ := forecastUsage(slow)
	assert.Equal(t, "after_reset", forecast.Status)
	assert.NotEmpty(t, forecast.HitsLimitAt, "the response still exposes the straight-line crossing for comparison")

	flat, _ := forecastUsage(usageHistoryRows(base, 10, 10, 10))
	assert.Equal(t, "flat", flat.Status)
	assert.Empty(t, flat.HitsLimitAt)
}
