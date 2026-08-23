package ratelimit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

func TestWaitForRateLimitUsesCacheWithoutRefreshing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	require.NoError(t, config.Save(&config.Config{RateLimit: &config.RateLimitConfig{
		FiveHourPercentMaxUsed: 99,
		SevenDayPercentMaxUsed: 99.9,
	}}))

	stamp := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	cached := usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 10, ResetsAt: time.Now().Add(time.Hour)},
		FetchedAt:     stamp,
		LastAttemptAt: stamp,
	}
	data, err := json.Marshal(cached)
	require.NoError(t, err)
	require.NoError(t, db.SaveUsageCache(data, stamp, stamp))

	assert.False(t, WaitForRateLimit(context.Background(), nil, "test", t.TempDir()))

	row, err := db.LoadUsageCache()
	require.NoError(t, err)
	var after usageapi.CachedUsage
	require.NoError(t, json.Unmarshal(row.Data, &after))
	assert.Equal(t, stamp, after.LastAttemptAt,
		"rate-limit gating must not attempt an API refresh")
}
