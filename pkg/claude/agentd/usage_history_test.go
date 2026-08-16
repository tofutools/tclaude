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
	native := []db.SubscriptionUsageHistoryRow{
		{Provider: db.SubscriptionProviderAnthropic, WindowName: "five_hour", ObservedAt: now.Add(-70 * time.Minute)},
		{Provider: db.SubscriptionProviderAnthropic, WindowName: "five_hour", ObservedAt: now.Add(-50 * time.Minute)},
	}
	warnings, err := collectOpenCodeUsageCoverageWarnings(now.Add(-24*time.Hour), nil, now, native)
	require.NoError(t, err)
	require.Len(t, warnings, 2, "Anthropic native coverage suppresses only the matching warning")
	assert.Equal(t, "openai", warnings[0].Provider)
	assert.Equal(t, "openai", warnings[0].NativeSource)
	assert.Equal(t, []string{"gpt-5"}, warnings[0].Models)
	assert.Equal(t, "openrouter", warnings[1].Provider)
	assert.Empty(t, warnings[1].NativeSource, "unknown providers have no fabricated native source")

	native = append(native,
		db.SubscriptionUsageHistoryRow{
			Provider: db.SubscriptionProviderOpenAI, WindowName: "five_hour",
			ObservedAt: now.Add(-70 * time.Minute),
		},
		db.SubscriptionUsageHistoryRow{
			Provider: db.SubscriptionProviderOpenAI, WindowName: "five_hour",
			ObservedAt: now.Add(-50 * time.Minute),
		},
	)
	warnings, err = collectOpenCodeUsageCoverageWarnings(now.Add(-24*time.Hour), nil, now, native)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Equal(t, "openrouter", warnings[0].Provider,
		"matching Codex/OpenAI history removes the OpenAI warning while unknown remains")
}

func TestOpenCodeUsageCoverageWarningClearsOnceNativeSamplingCatchesUp(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, db.UpsertOpenCodeUsageActivity(db.OpenCodeUsageActivity{
		SessionID: "oc-openai", MessageID: "m1", ConvID: "c1",
		ProviderID: "openai", ModelID: "gpt-5", ObservedAt: now.Add(-time.Hour),
	}))
	warnings, err := collectOpenCodeUsageCoverageWarnings(
		now.Add(-24*time.Hour), nil, now,
		[]db.SubscriptionUsageHistoryRow{{
			Provider: db.SubscriptionProviderOpenAI, WindowName: "five_hour",
			ObservedAt: now.Add(-30 * time.Minute),
		}},
	)
	require.NoError(t, err)
	assert.Empty(t, warnings,
		"a native sample taken after the OpenCode turn already contains that turn's spend")

	warnings, err = collectOpenCodeUsageCoverageWarnings(
		now.Add(-24*time.Hour), nil, now,
		[]db.SubscriptionUsageHistoryRow{{
			Provider: db.SubscriptionProviderOpenAI, WindowName: "five_hour",
			ObservedAt: now.Add(-2 * time.Hour),
		}},
	)
	require.NoError(t, err)
	require.Len(t, warnings, 1, "activity newer than the last native sample is missing from the graphs")
	assert.Equal(t, db.SubscriptionProviderOpenAI, warnings[0].Provider)
	assert.Equal(t, now.Add(-2*time.Hour).Format(time.RFC3339Nano), warnings[0].NativeLatest)
	assert.Equal(t, now.Add(-time.Hour).Format(time.RFC3339Nano), warnings[0].ActivityTo)
}

func TestOpenCodeUsageCoverageWarningIgnoresSamplingGapsBehindTheNewestSample(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	activity := base.Add(time.Hour)
	assert.True(t, openCodeActivityCoveredByNativeSamples(activity, activity, activity.Add(time.Hour)),
		"a sample taken at the turn covers it however overdue the next sample is")
	assert.True(t, openCodeActivityCoveredByNativeSamples(activity, base.Add(2*time.Hour), base.Add(9*time.Hour)),
		"any later sample covers the activity outright")
	assert.True(t, openCodeActivityCoveredByNativeSamples(
		base.Add(14*24*time.Hour), base.Add(14*24*time.Hour+time.Minute), base.Add(14*24*time.Hour+2*time.Minute),
	), "a mid-span sampling gap does not invalidate a fresh newest sample")

	// The grace forgives an uncovered turn only while the sampler is still
	// within one grace window of now, and it expires exactly there.
	native := base.Add(50 * time.Minute)
	assert.True(t, openCodeActivityCoveredByNativeSamples(activity, native, native.Add(openCodeCoverageGrace)),
		"a sample due but not yet taken is in flight, not missing coverage")
	assert.False(t, openCodeActivityCoveredByNativeSamples(activity, native, native.Add(openCodeCoverageGrace+time.Second)),
		"once the sampler is overdue past the grace the uncovered turn is missing for good")
	assert.False(t, openCodeActivityCoveredByNativeSamples(activity, native, base.Add(72*time.Hour)),
		"a stale sample does not keep forgiving the turn days later")
	assert.False(t, openCodeActivityCoveredByNativeSamples(activity, base.Add(30*time.Minute), activity),
		"a sample half an hour stale cannot include usage that came after it")

	assert.False(t, openCodeActivityCoveredByNativeSamples(activity, time.Time{}, activity),
		"no native sample at all is never coverage")
	assert.False(t, openCodeActivityCoveredByNativeSamples(time.Time{}, native, activity),
		"no activity is not something a sample can cover")
}

func TestOpenCodeUsageCoverageWarningsUseProviderSelectedSpan(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, db.UpsertOpenCodeUsageActivity(db.OpenCodeUsageActivity{
		SessionID: "oc-openai", MessageID: "m1", ConvID: "c1",
		ProviderID: "openai", ModelID: "gpt-5", ObservedAt: now.Add(-48 * time.Hour),
	}))

	warnings, err := collectOpenCodeUsageCoverageWarnings(
		now.Add(-7*24*time.Hour),
		map[string]time.Time{
			db.SubscriptionProviderOpenAI:    now.Add(-24 * time.Hour),
			db.SubscriptionProviderAnthropic: now.Add(-7 * 24 * time.Hour),
		},
		now,
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, warnings,
		"Anthropic's longer selected span must not pull old activity into OpenAI's 24-hour warning")
}

func TestCollectUsageHistoryHonorsShorterProviderSpanForCoverage(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := db.SaveSubscriptionUsageSample(db.SubscriptionUsageSample{
		Provider: db.SubscriptionProviderOpenAI, ObservedAt: now.Add(-48 * time.Hour), Source: "codex",
		Windows: []db.SubscriptionUsageWindow{{Name: "five_hour", UsedPercent: 10}},
	})
	require.NoError(t, err)
	require.NoError(t, db.UpsertOpenCodeUsageActivity(db.OpenCodeUsageActivity{
		SessionID: "oc-openai", MessageID: "m1", ConvID: "c1",
		ProviderID: "openai", ModelID: "gpt-5", ObservedAt: now.Add(-12 * time.Hour),
	}))

	out, err := collectUsageHistory(
		now.Add(-7*24*time.Hour),
		map[usageSeriesKey]time.Time{
			{provider: db.SubscriptionProviderOpenAI, window: "five_hour"}: now.Add(-24 * time.Hour),
		},
		now,
	)
	require.NoError(t, err)
	require.Len(t, out.CoverageWarnings, 1,
		"native history outside the selected 24-hour card span does not qualify current OpenCode activity")
	assert.Equal(t, db.SubscriptionProviderOpenAI, out.CoverageWarnings[0].Provider)
	assert.Equal(t, now.Add(-24*time.Hour).Format(time.RFC3339Nano), out.CoverageWarnings[0].From)
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

	forecasts, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt, base)
	require.Len(t, resets, 1)
	assert.Equal(t, 20.0, resets[0].Pct, "the observed post-reset minimum is retained; no synthetic zero")
	forecast := forecasts[usageForecastAlgoFit]
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

	forecasts, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt, base)
	require.Len(t, resets, 1, "a crossed declared boundary is recorded exactly once")
	forecast := forecasts[usageForecastDefaultAlgo]
	assert.Equal(t, 15.0, forecast.BaselinePct)
	assert.Equal(t, 3, forecast.SampleCount)
	assert.Equal(t, "before_reset", forecast.Status)
}

func TestForecastUsageRequiresEnoughPostResetHistory(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 12, 13)
	forecasts, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt, base)
	assert.Empty(t, resets)
	for algo, forecast := range forecasts {
		assert.Equal(t, "insufficient", forecast.Status, algo)
		assert.Zero(t, forecast.RatePctPerHour, algo)
	}
}

func TestForecastUsageReportsResetFirstAndFlatPaces(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	slow := usageHistoryRows(base, 10, 11, 12)
	for i := range slow {
		slow[i].ResetsAt = base.Add(time.Hour)
	}
	forecasts, _ := forecastUsage(slow, slow[len(slow)-1].ObservedAt, base)
	forecast := forecasts[usageForecastDefaultAlgo]
	assert.Equal(t, "after_reset", forecast.Status)
	assert.NotEmpty(t, forecast.HitsLimitAt, "the response still exposes the straight-line crossing for comparison")

	flatRows := usageHistoryRows(base, 10, 10, 10)
	flatForecasts, _ := forecastUsage(flatRows, flatRows[len(flatRows)-1].ObservedAt, base)
	for algo, flat := range flatForecasts {
		assert.Equal(t, "flat", flat.Status, algo)
		assert.Empty(t, flat.HitsLimitAt, algo)
	}
}

func TestForecastUsagePausesStaleReadings(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 10, 15, 20)
	forecasts, _ := forecastUsage(rows, rows[len(rows)-1].ObservedAt.Add(usageForecastStaleAfter+time.Minute), base)
	forecast := forecasts[usageForecastDefaultAlgo]
	assert.Equal(t, "stale", forecast.Status, "an old sample cannot masquerade as a current pace")
	assert.Empty(t, forecast.HitsLimitAt)

	for i := range rows {
		rows[i].ResetsAt = rows[len(rows)-1].ObservedAt
	}
	forecasts, _ = forecastUsage(rows, rows[len(rows)-1].ObservedAt.Add(time.Minute), base)
	assert.Equal(t, "stale", forecasts[usageForecastDefaultAlgo].Status,
		"a passed declared reset invalidates the old percentage immediately")
}

// An idle stretch followed by a burst is the shape that made the old
// least-squares-only forecast unreadable: it weights the idle hours so heavily
// that the drawn line barely rises while usage is climbing steeply.
func usageIdleThenBurstRows(base time.Time) []db.SubscriptionUsageHistoryRow {
	values := make([]float64, 0, 37)
	for range 32 { // 8 idle hours at 10%.
		values = append(values, 10)
	}
	for i := 1; i <= 4; i++ { // One hour climbing to 50%.
		values = append(values, 10+float64(i)*10)
	}
	return usageHistoryRows(base, values...)
}

func TestForecastUsageAlgorithmsDisagreeOnIdleThenBurst(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageIdleThenBurstRows(base)
	last := rows[len(rows)-1].ObservedAt

	forecasts, resets := forecastUsage(rows, last, base)
	require.Empty(t, resets)
	require.Len(t, forecasts, 3)

	// Span: first to last sample of the view, 40 points over 8h45m.
	assert.InDelta(t, 40.0/8.75, forecasts[usageForecastAlgoSpan].RatePctPerHour, 1e-9)
	assert.Equal(t, len(rows), forecasts[usageForecastAlgoSpan].SampleCount)
	// Recent: the newest five samples, which is the burst alone.
	assert.InDelta(t, 40.0, forecasts[usageForecastAlgoRecent].RatePctPerHour, 1e-9)
	assert.Equal(t, usageForecastRecentCount, forecasts[usageForecastAlgoRecent].SampleCount)
	// Fit lags both by design; the burst is a fraction of the segment it averages.
	assert.Less(t, forecasts[usageForecastAlgoFit].RatePctPerHour, forecasts[usageForecastAlgoSpan].RatePctPerHour)

	for algo, forecast := range forecasts {
		assert.Equal(t, algo, forecast.Algorithm)
		assert.Equal(t, "projected", forecast.Status, algo)
	}
}

func TestForecastUsageSpanHonorsTheCardsHistoryRange(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageIdleThenBurstRows(base)
	last := rows[len(rows)-1].ObservedAt

	// A card narrowed to the burst asks for the burst's pace: the samples before
	// its view start are excluded from the span line, but not from the segment
	// the other algorithms average.
	forecasts, _ := forecastUsage(rows, last, last.Add(-time.Hour))
	assert.InDelta(t, 40.0, forecasts[usageForecastAlgoSpan].RatePctPerHour, 1e-9)
	assert.Equal(t, 5, forecasts[usageForecastAlgoSpan].SampleCount)
	assert.Equal(t, 10.0, forecasts[usageForecastAlgoSpan].BaselinePct)
	assert.Less(t, forecasts[usageForecastAlgoFit].RatePctPerHour, forecasts[usageForecastAlgoSpan].RatePctPerHour)

	// A view that starts after every sample has nothing to draw a line through.
	forecasts, _ = forecastUsage(rows, last, last.Add(time.Minute))
	assert.Equal(t, "insufficient", forecasts[usageForecastAlgoSpan].Status)
	assert.Zero(t, forecasts[usageForecastAlgoSpan].SampleCount)
	assert.Equal(t, "projected", forecasts[usageForecastAlgoRecent].Status,
		"the other algorithms are anchored on the segment, not the view")
}

func TestForecastUsageRecentReachesBackForEnoughElapsedTime(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rows := usageHistoryRows(base, 10, 12, 14, 16, 18, 20)
	// Squeeze the newest five samples into twenty minutes, as a burst of turns
	// does — less than the minimum elapsed time a pace may be divided by.
	for i := 1; i < len(rows); i++ {
		rows[i].ObservedAt = base.Add(time.Duration(i-1)*5*time.Minute + time.Hour)
	}
	last := rows[len(rows)-1].ObservedAt

	forecasts, _ := forecastUsage(rows, last, base)
	recent := forecasts[usageForecastAlgoRecent]
	assert.Equal(t, len(rows), recent.SampleCount,
		"the window widens until it covers the minimum elapsed time")
	assert.InDelta(t, 10.0/(last.Sub(base).Hours()), recent.RatePctPerHour, 1e-9)
}

func TestForecastUsageStraightLineIgnoresDownwardNoise(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	// A sub-threshold dip is noise rather than a reset, so it stays in the
	// segment; a negative pace would still be nonsense to draw.
	rows := usageHistoryRows(base, 20, 19.5, 19, 18.5)
	forecasts, resets := forecastUsage(rows, rows[len(rows)-1].ObservedAt, base)
	assert.Empty(t, resets)
	for algo, forecast := range forecasts {
		assert.Equal(t, "flat", forecast.Status, algo)
		assert.Zero(t, forecast.RatePctPerHour, algo)
	}
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
