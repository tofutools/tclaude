package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestOpenCodeUsageCoverageWarningsAreProviderAware(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, row := range []db.OpenCodeUsageActivity{
		{SessionID: "oc-openai", MessageID: "m1", ConvID: "c1", ProviderID: "openai", ModelID: "gpt-5", ObservedAt: now.Add(-time.Hour)},
		{SessionID: "oc-anthropic", MessageID: "m2", ConvID: "c2", ProviderID: "anthropic", ModelID: "claude-sonnet", ObservedAt: now.Add(-time.Hour)},
		{SessionID: "oc-unknown", MessageID: "m3", ConvID: "c3", ProviderID: "openrouter", ModelID: "some-model", ObservedAt: now.Add(-time.Hour)},
	} {
		require.NoError(t, db.UpsertOpenCodeUsageActivity(row))
	}
	native := []db.SubscriptionUsageHistoryRow{{
		Provider: db.SubscriptionProviderAnthropic, WindowName: "five_hour",
		ObservedAt: now.Add(-30 * time.Minute),
	}}
	warnings, err := collectOpenCodeUsageCoverageWarnings(now.Add(-24*time.Hour), now, native)
	require.NoError(t, err)
	require.Len(t, warnings, 2, "Anthropic native coverage suppresses only the matching warning")
	assert.Equal(t, "openai", warnings[0].Provider)
	assert.Equal(t, "openai", warnings[0].NativeSource)
	assert.Equal(t, []string{"gpt-5"}, warnings[0].Models)
	assert.Equal(t, "openrouter", warnings[1].Provider)
	assert.Empty(t, warnings[1].NativeSource, "unknown providers have no fabricated native source")

	native = append(native, db.SubscriptionUsageHistoryRow{
		Provider: db.SubscriptionProviderOpenAI, WindowName: "five_hour",
		ObservedAt: now.Add(-20 * time.Minute),
	})
	warnings, err = collectOpenCodeUsageCoverageWarnings(now.Add(-24*time.Hour), now, native)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Equal(t, "openrouter", warnings[0].Provider,
		"matching Codex/OpenAI history removes the OpenAI warning while unknown remains")
}

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

	forecast, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt)
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

	forecast, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt)
	require.Len(t, resets, 1, "a crossed declared boundary is recorded exactly once")
	assert.Equal(t, 15.0, forecast.BaselinePct)
	assert.Equal(t, 3, forecast.SampleCount)
	assert.Equal(t, "before_reset", forecast.Status)
}

func TestForecastUsageRequiresEnoughPostResetHistory(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 12, 13)
	forecast, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt)
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
	forecast, _ := forecastUsage(slow, slow[len(slow)-1].ObservedAt)
	assert.Equal(t, "after_reset", forecast.Status)
	assert.NotEmpty(t, forecast.HitsLimitAt, "the response still exposes the straight-line crossing for comparison")

	flatRows := usageHistoryRows(base, 10, 10, 10)
	flat, _ := forecastUsage(flatRows, flatRows[len(flatRows)-1].ObservedAt)
	assert.Equal(t, "flat", flat.Status)
	assert.Empty(t, flat.HitsLimitAt)
}

func TestForecastUsagePausesStaleReadings(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 10, 15, 20)
	forecast, _ := forecastUsage(rows, rows[len(rows)-1].ObservedAt.Add(usageForecastStaleAfter+time.Minute))
	assert.Equal(t, "stale", forecast.Status, "an old sample cannot masquerade as a current pace")
	assert.Empty(t, forecast.HitsLimitAt)

	for i := range rows {
		rows[i].ResetsAt = rows[len(rows)-1].ObservedAt
	}
	forecast, _ = forecastUsage(rows, rows[len(rows)-1].ObservedAt.Add(time.Minute))
	assert.Equal(t, "stale", forecast.Status, "a passed declared reset invalidates the old percentage immediately")
}

func TestDownsampleUsageRowsBoundsWireAndPreservesReset(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, make([]float64, 2000)...)
	for i := range rows {
		rows[i].UsedPercent = float64(i % 100)
	}
	resetIndex := 1000
	resets := []usageHistoryReset{{
		At: rows[resetIndex].ObservedAt.Format(time.RFC3339Nano), Pct: rows[resetIndex].UsedPercent,
	}}
	downsampled := downsampleUsageRows(rows, resets, 120)
	require.LessOrEqual(t, len(downsampled), 120)
	assert.Equal(t, rows[0].ObservedAt, downsampled[0].ObservedAt)
	assert.Equal(t, rows[len(rows)-1].ObservedAt, downsampled[len(downsampled)-1].ObservedAt)
	present := map[time.Time]bool{}
	for _, row := range downsampled {
		present[row.ObservedAt] = true
	}
	assert.True(t, present[rows[resetIndex-1].ObservedAt], "pre-reset point retained so the old segment has an endpoint")
	assert.True(t, present[rows[resetIndex].ObservedAt], "post-reset minimum retained so the new segment has a baseline")
}

func TestDownsampleUsageRowsPreservesLatestIncludedPoint(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, make([]float64, 2001)...)
	for i := range rows {
		rows[i].UsedPercent = float64(i % 100)
	}
	rows[len(rows)-1].Excluded = true

	downsampled := downsampleUsageRows(rows, nil, 1200)
	present := map[time.Time]bool{}
	for _, row := range downsampled {
		present[row.ObservedAt] = true
	}
	assert.True(t, present[rows[len(rows)-1].ObservedAt], "excluded latest point remains reversible")
	assert.True(t, present[rows[len(rows)-2].ObservedAt],
		"latest included point remains the displayed current value and forecast anchor")
}

func TestDownsampleUsageRowsPreservesPreviousIncludedPointAtReset(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, make([]float64, 2001)...)
	resetIndex := 1000
	rows[resetIndex-1].Excluded = true
	resets := []usageHistoryReset{{
		At: rows[resetIndex].ObservedAt.Format(time.RFC3339Nano), Pct: rows[resetIndex].UsedPercent,
	}}

	downsampled := downsampleUsageRows(rows, resets, 1200)
	present := map[time.Time]bool{}
	for _, row := range downsampled {
		present[row.ObservedAt] = true
	}
	assert.True(t, present[rows[resetIndex-2].ObservedAt],
		"pre-reset line retains its true included endpoint when the adjacent point is excluded")
}

func TestDownsampleUsageResetsBoundsMarkers(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	resets := make([]usageHistoryReset, 2000)
	for i := range resets {
		resets[i] = usageHistoryReset{At: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)}
	}
	downsampled := downsampleUsageResets(resets, 120)
	require.LessOrEqual(t, len(downsampled), 120)
	assert.Equal(t, resets[0].At, downsampled[0].At)
	assert.Equal(t, resets[len(resets)-1].At, downsampled[len(downsampled)-1].At)
}
