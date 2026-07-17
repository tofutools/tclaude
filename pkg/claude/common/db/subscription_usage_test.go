package db

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveSubscriptionUsageSampleCoalescesFifteenMinuteBucket(t *testing.T) {
	setupTestDB(t)
	base := time.Now().UTC().Truncate(time.Hour)
	first := SubscriptionUsageSample{
		Provider:   SubscriptionProviderAnthropic,
		ObservedAt: base.Add(2 * time.Minute),
		Source:     "statusline",
		Windows: []SubscriptionUsageWindow{
			{Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: 10, ResetsAt: base.Add(4 * time.Hour)},
			{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: 20, ResetsAt: base.Add(6 * 24 * time.Hour)},
		},
	}
	stored, err := SaveSubscriptionUsageSample(first)
	require.NoError(t, err)
	assert.True(t, stored)

	older := first
	older.ObservedAt = base.Add(time.Minute)
	older.Windows = []SubscriptionUsageWindow{{Name: "five_hour", UsedPercent: 99}}
	stored, err = SaveSubscriptionUsageSample(older)
	require.NoError(t, err)
	assert.False(t, stored, "an older observation cannot regress its bucket")

	newer := first
	newer.ObservedAt = base.Add(14 * time.Minute)
	newer.Source = "api"
	newer.Windows = []SubscriptionUsageWindow{{Name: "seven_day", Duration: 7 * 24 * time.Hour, UsedPercent: 23}}
	stored, err = SaveSubscriptionUsageSample(newer)
	require.NoError(t, err)
	assert.True(t, stored, "newest observation replaces its bucket")

	d, err := Open()
	require.NoError(t, err)
	var samples, windows int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_samples`).Scan(&samples))
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_windows`).Scan(&windows))
	assert.Equal(t, 1, samples)
	assert.Equal(t, 2, windows, "a newer partial reading preserves other genuinely observed windows")
	var sampledAt, observedAt, source, name string
	var duration int64
	var pct float64
	require.NoError(t, d.QueryRow(`SELECT s.sampled_at, w.observed_at, w.source,
		w.window_name, w.duration_seconds, w.used_percent
		FROM subscription_usage_samples s JOIN subscription_usage_windows w ON w.sample_id = s.id
		WHERE w.window_name = 'seven_day'`).
		Scan(&sampledAt, &observedAt, &source, &name, &duration, &pct))
	assert.Equal(t, base.Format(time.RFC3339Nano), sampledAt)
	assert.Equal(t, newer.ObservedAt.Format(time.RFC3339Nano), observedAt)
	assert.Equal(t, "api", source)
	assert.Equal(t, "seven_day", name)
	assert.Equal(t, int64((7*24*time.Hour)/time.Second), duration)
	assert.Equal(t, 23.0, pct)
	var fiveHourPct float64
	var fiveHourSource string
	require.NoError(t, d.QueryRow(`SELECT used_percent, source FROM subscription_usage_windows
		WHERE window_name = 'five_hour'`).Scan(&fiveHourPct, &fiveHourSource))
	assert.Equal(t, 10.0, fiveHourPct, "omitted five-hour window keeps its genuine earlier observation")
	assert.Equal(t, "statusline", fiveHourSource)
}

func TestSaveSubscriptionUsageSampleSeparatesProvidersAndBuckets(t *testing.T) {
	setupTestDB(t)
	base := time.Now().UTC().Truncate(time.Hour)
	for _, sample := range []SubscriptionUsageSample{
		{Provider: SubscriptionProviderAnthropic, ObservedAt: base.Add(time.Minute), Windows: []SubscriptionUsageWindow{{Name: "five_hour", UsedPercent: 1}}},
		{Provider: SubscriptionProviderOpenAI, ObservedAt: base.Add(2 * time.Minute), Windows: []SubscriptionUsageWindow{{Name: "seven_day", UsedPercent: 2}}},
		{Provider: SubscriptionProviderOpenAI, ObservedAt: base.Add(16 * time.Minute), Windows: []SubscriptionUsageWindow{{Name: "seven_day", UsedPercent: 3}}},
	} {
		stored, err := SaveSubscriptionUsageSample(sample)
		require.NoError(t, err)
		assert.True(t, stored)
	}
	d, err := Open()
	require.NoError(t, err)
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_samples`).Scan(&count))
	assert.Equal(t, 3, count)
}

func TestPruneSubscriptionUsageHistoryCascadesWindows(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	now := time.Now().UTC()
	oldResult, err := d.Exec(`INSERT INTO subscription_usage_samples(provider, sampled_at)
		VALUES ('anthropic', ?)`, now.Add(-100*24*time.Hour).Format(time.RFC3339Nano))
	require.NoError(t, err)
	oldID, err := oldResult.LastInsertId()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO subscription_usage_windows(sample_id, window_name, used_percent, observed_at)
		VALUES (?, 'seven_day', 40, ?)`, oldID, now.Add(-100*24*time.Hour).Format(time.RFC3339Nano))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO subscription_usage_samples(provider, sampled_at)
		VALUES ('anthropic', ?)`, now.Add(-time.Hour).Format(time.RFC3339Nano))
	require.NoError(t, err)

	deleted, err := PruneSubscriptionUsageHistory(now.Add(-DefaultSubscriptionUsageRetention))
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	var samples, windows int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_samples`).Scan(&samples))
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_windows`).Scan(&windows))
	assert.Equal(t, 1, samples)
	assert.Zero(t, windows)
}

func TestSaveSubscriptionUsageSampleRejectsInvalidAndExpiredReadings(t *testing.T) {
	setupTestDB(t)
	validWindow := []SubscriptionUsageWindow{{Name: "seven_day", UsedPercent: 1}}
	for _, sample := range []SubscriptionUsageSample{
		{ObservedAt: time.Now(), Windows: validWindow},
		{Provider: "openai", Windows: validWindow},
		{Provider: "openai", ObservedAt: time.Now()},
		{Provider: "openai", ObservedAt: time.Now(), Windows: []SubscriptionUsageWindow{{Name: "", UsedPercent: 1}}},
		{Provider: "openai", ObservedAt: time.Now(), Windows: []SubscriptionUsageWindow{{Name: "x", Duration: -time.Second, UsedPercent: 1}}},
		{Provider: "openai", ObservedAt: time.Now(), Windows: []SubscriptionUsageWindow{{Name: "x", UsedPercent: math.NaN()}}},
		{Provider: "openai", ObservedAt: time.Now(), Windows: []SubscriptionUsageWindow{{Name: "x"}, {Name: "x"}}},
	} {
		stored, err := SaveSubscriptionUsageSample(sample)
		assert.False(t, stored)
		assert.Error(t, err)
	}
	stored, err := SaveSubscriptionUsageSample(SubscriptionUsageSample{
		Provider: "openai", ObservedAt: time.Now().Add(-DefaultSubscriptionUsageRetention - time.Hour), Windows: validWindow,
	})
	require.NoError(t, err)
	assert.False(t, stored, "expired delayed observations are ignored")
}
