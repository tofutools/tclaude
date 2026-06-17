package usageapi

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setupTestCache sets up an isolated SQLite DB for cache testing.
func setupTestCache(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	db.ResetForTest()
}

// stubFuncs replaces fetchFunc, getTokenFunc, and refreshTokenFunc for the duration of a test.
// Pass nil for refresh to use a default that always fails (no refresh available).
func stubFuncs(t *testing.T, token func() (string, error), fetch func(string) (*Response, error)) {
	t.Helper()
	stubFuncsWithRefresh(t, token, fetch, func() (string, error) {
		return "", fmt.Errorf("no refresh token configured in test")
	})
}

func stubFuncsWithRefresh(t *testing.T, token func() (string, error), fetch func(string) (*Response, error), refresh func() (string, error)) {
	t.Helper()
	origFetch := fetchFunc
	origToken := getTokenFunc
	origRefresh := refreshTokenFunc
	fetchFunc = fetch
	getTokenFunc = token
	refreshTokenFunc = refresh
	t.Cleanup(func() {
		fetchFunc = origFetch
		getTokenFunc = origToken
		refreshTokenFunc = origRefresh
	})
}

func okToken() (string, error) { return "test-token", nil }

func okFetch(pct float64) func(string) (*Response, error) {
	return func(string) (*Response, error) {
		return &Response{
			FiveHour: &Bucket{Utilization: pct, ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		}, nil
	}
}

func failFetch(_ string) (*Response, error) {
	return nil, fmt.Errorf("API returned 500: server error")
}

func rateLimitFetch(_ string) (*Response, error) {
	return nil, &RateLimitError{Body: `{"error":{"message":"Rate limited.","type":"rate_limit_error"}}`}
}

func TestGetCached_FreshCacheSkipsFetch(t *testing.T) {
	setupTestCache(t)

	var fetchCount atomic.Int32
	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return okFetch(42.0)(token)
	})

	// First call should fetch
	result, err := GetCached()
	require.NoError(t, err)
	require.NotNil(t, result.FiveHour, "expected non-nil FiveHour")
	require.Equal(t, 42.0, result.FiveHour.Pct, "expected 42%%, got %+v", result.FiveHour)

	// Second call within TTL should use cache, not fetch again
	result2, err := GetCached()
	require.NoError(t, err)
	require.NotNil(t, result2.FiveHour, "expected non-nil FiveHour")
	require.Equal(t, 42.0, result2.FiveHour.Pct, "expected 42%%, got %+v", result2.FiveHour)

	assert.Equal(t, int32(1), fetchCount.Load(), "expected 1 fetch call")
}

func TestGetCached_ErrorReturnsStaleCacheAndError(t *testing.T) {
	setupTestCache(t)

	// Seed the cache with a successful fetch
	stubFuncs(t, okToken, okFetch(50.0))
	_, err := GetCached()
	require.NoError(t, err, "seed fetch failed")

	// Expire the cache by backdating LastAttemptAt
	stale := loadCacheStale()
	require.NotNil(t, stale, "expected stale cache")
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	// Now make fetch fail
	stubFuncs(t, okToken, failFetch)
	result, err := GetCached()
	require.Error(t, err, "expected error")
	require.NotNil(t, result, "expected stale data")
	require.NotNil(t, result.FiveHour, "expected non-nil FiveHour")
	require.Equal(t, 50.0, result.FiveHour.Pct, "expected stale 50%%, got %+v", result.FiveHour)
}

func TestGetCached_BackoffAfterError(t *testing.T) {
	setupTestCache(t)

	// Seed cache
	stubFuncs(t, okToken, okFetch(25.0))
	_, err := GetCached()
	require.NoError(t, err, "seed fetch failed")

	// Expire
	stale := loadCacheStale()
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	// Fail once — should stamp LastAttemptAt
	var fetchCount atomic.Int32
	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return failFetch(token)
	})

	_, _ = GetCached()

	// Second call should NOT fetch — backoff is active
	_, _ = GetCached()

	assert.Equal(t, int32(1), fetchCount.Load(), "expected 1 fetch during backoff")
}

func TestRefreshCache_BackoffAfterError(t *testing.T) {
	setupTestCache(t)

	// Seed cache
	stubFuncs(t, okToken, okFetch(30.0))
	RefreshCache()

	// Expire
	stale := loadCacheStale()
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	// Fail once
	var fetchCount atomic.Int32
	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return failFetch(token)
	})

	RefreshCache()

	// Second call should be backed off
	RefreshCache()

	assert.Equal(t, int32(1), fetchCount.Load(), "expected 1 fetch during backoff")
}

func TestGetCached_NoStaleCache_ReturnsNilAndError(t *testing.T) {
	setupTestCache(t)

	stubFuncs(t, okToken, failFetch)

	result, err := GetCached()
	require.Error(t, err, "expected error")
	// No prior successful fetch, so no usage data to return
	assert.Nil(t, result, "expected nil result")
}

func TestLoadCacheStale_NoCacheFile_ReturnsNil(t *testing.T) {
	setupTestCache(t)
	// No cache file exists — loadCacheStale should return nil
	stale := loadCacheStale()
	require.Nil(t, stale, "expected nil")
}

func TestLoadCacheWithTTL_RespectsLastAttemptAt(t *testing.T) {
	setupTestCache(t)

	// Write a cache entry with FetchedAt old but LastAttemptAt recent
	cached := &CachedUsage{
		FetchedAt:     time.Now().Add(-time.Hour),
		LastAttemptAt: time.Now(),
		FiveHour:      &CachedBucket{Pct: 10.0},
	}
	saveCache(cached)

	// Should be considered fresh (LastAttemptAt is recent)
	result := loadCache()
	require.NotNil(t, result, "expected fresh cache based on LastAttemptAt")

	// Now backdate LastAttemptAt too
	cached.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(cached)

	result = loadCache()
	require.Nil(t, result, "expected expired cache")
}

func TestStampLastAttempt_NoCacheFile_CreatesMinimalEntry(t *testing.T) {
	setupTestCache(t)

	stampLastAttempt(fmt.Errorf("test error"))

	// Should create a minimal cache entry with just LastAttemptAt
	cached := loadCacheStale()
	require.NotNil(t, cached, "expected minimal cache entry")
	require.True(t, cached.FiveHour == nil && cached.SevenDay == nil, "expected no usage data in minimal entry")
	require.LessOrEqual(t, time.Since(cached.LastAttemptAt), time.Second, "expected recent LastAttemptAt")
}

func TestGetCached_BackoffEvenWithoutStaleCache(t *testing.T) {
	setupTestCache(t)

	var fetchCount atomic.Int32
	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return failFetch(token)
	})

	// First call fails, no stale cache
	_, _ = GetCached()

	// Second call should NOT fetch — backoff is active even without stale data
	_, _ = GetCached()

	assert.Equal(t, int32(1), fetchCount.Load(), "expected 1 fetch during backoff")
}

func TestGetCached_429ReturnsErrorByDefault(t *testing.T) {
	setupTestCache(t)

	// Seed cache so we get stale fallback
	stubFuncs(t, okToken, okFetch(42.0))
	_, err := GetCached()
	require.NoError(t, err, "seed fetch failed")
	stale := loadCacheStale()
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	stubFuncs(t, okToken, rateLimitFetch)

	result, err := GetCached()
	require.Error(t, err, "expected error on 429")
	require.NotNil(t, result, "expected stale 42%%, got %+v", result)
	require.NotNil(t, result.FiveHour, "expected stale 42%%, got %+v", result)
	require.Equal(t, 42.0, result.FiveHour.Pct, "expected stale 42%%, got %+v", result)
}

func TestGetCached_429RefreshesTokenWhenEnvSet(t *testing.T) {
	setupTestCache(t)
	t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")

	var fetchCount atomic.Int32
	var refreshCount atomic.Int32

	stubFuncsWithRefresh(t, okToken,
		func(token string) (*Response, error) {
			n := fetchCount.Add(1)
			if n == 1 {
				return rateLimitFetch(token)
			}
			return okFetch(77.0)(token)
		},
		func() (string, error) {
			refreshCount.Add(1)
			return "refreshed-token", nil
		},
	)

	result, err := GetCached()
	require.NoError(t, err, "expected success after refresh")
	require.NotNil(t, result.FiveHour, "expected 77%%, got %+v", result.FiveHour)
	require.Equal(t, 77.0, result.FiveHour.Pct, "expected 77%%, got %+v", result.FiveHour)
	assert.Equal(t, int32(2), fetchCount.Load(), "expected 2 fetches (original + retry)")
	assert.Equal(t, int32(1), refreshCount.Load(), "expected 1 refresh")
}

func TestGetCached_429RefreshFailsFallsBackToStale(t *testing.T) {
	setupTestCache(t)

	// Seed cache
	stubFuncs(t, okToken, okFetch(33.0))
	_, err := GetCached()
	require.NoError(t, err, "seed fetch failed")

	// Expire cache
	stale := loadCacheStale()
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	// Now return 429 and fail the refresh too
	stubFuncsWithRefresh(t, okToken, rateLimitFetch,
		func() (string, error) {
			return "", fmt.Errorf("refresh token expired")
		},
	)

	result, err := GetCached()
	require.Error(t, err, "expected error")
	require.NotNil(t, result, "expected stale data")
	require.NotNil(t, result.FiveHour, "expected stale 33%%, got %+v", result.FiveHour)
	require.Equal(t, 33.0, result.FiveHour.Pct, "expected stale 33%%, got %+v", result.FiveHour)
}

func TestRefreshCache_429DoesNotRetryByDefault(t *testing.T) {
	setupTestCache(t)

	var fetchCount atomic.Int32

	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return rateLimitFetch(token)
	})

	RefreshCache()

	assert.Equal(t, int32(1), fetchCount.Load(), "expected 1 fetch (no retry)")
}

func TestRefreshCache_429RefreshesTokenWhenEnvSet(t *testing.T) {
	setupTestCache(t)
	t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")

	var fetchCount atomic.Int32

	stubFuncsWithRefresh(t, okToken,
		func(token string) (*Response, error) {
			n := fetchCount.Add(1)
			if n == 1 {
				return rateLimitFetch(token)
			}
			return okFetch(55.0)(token)
		},
		func() (string, error) {
			return "refreshed-token", nil
		},
	)

	RefreshCache()

	cached := loadCacheStale()
	require.NotNil(t, cached, "expected cache to be populated after refresh+retry")
	require.NotNil(t, cached.FiveHour, "expected 55%%, got %+v", cached.FiveHour)
	require.Equal(t, 55.0, cached.FiveHour.Pct, "expected 55%%, got %+v", cached.FiveHour)
	assert.Equal(t, int32(2), fetchCount.Load(), "expected 2 fetches")
}

func TestCarryForwardWindow(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	future := &CachedBucket{Pct: 40, ResetsAt: now.Add(3 * 24 * time.Hour)}
	fresh := &CachedBucket{Pct: 12, ResetsAt: now.Add(2 * time.Hour)}

	tests := []struct {
		name       string
		fresh      *CachedBucket
		prev       *CachedBucket
		wantBucket *CachedBucket // expected identity, nil to expect a dropped window
	}{
		{"fresh reading always wins", fresh, future, fresh},
		{"fresh wins even over nil prev", fresh, nil, fresh},
		{"omitted + nonzero unreset prev → carried forward", nil, future, future},
		{"omitted + nil prev → dropped", nil, nil, nil},
		{"omitted + zero-usage prev → dropped", nil, &CachedBucket{Pct: 0, ResetsAt: now.Add(time.Hour)}, nil},
		{"omitted + past-reset prev → dropped", nil, &CachedBucket{Pct: 40, ResetsAt: now.Add(-time.Minute)}, nil},
		{"omitted + no-reset prev → dropped", nil, &CachedBucket{Pct: 40}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := carryForwardWindow(tc.fresh, tc.prev, now)
			if tc.wantBucket == nil {
				require.Nil(t, got, "expected window dropped")
				return
			}
			require.Same(t, tc.wantBucket, got, "expected the exact bucket carried/kept")
		})
	}
}

// A subsequent statusline render that omits the 7d (weekly) bucket — the
// API drops it when it has nothing fresh to report — must not blank out
// the dashboard's 7d bar: UpdateFromStatusLine carries the last-known
// nonzero, unreset window forward.
func TestUpdateFromStatusLine_CarriesForwardOmittedSevenDay(t *testing.T) {
	setupTestCache(t)

	now := time.Now()
	fh := &CachedBucket{Pct: 20, ResetsAt: now.Add(2 * time.Hour)}
	sd := &CachedBucket{Pct: 35, ResetsAt: now.Add(4 * 24 * time.Hour)}

	// First render carries both windows.
	UpdateFromStatusLine(fh, sd, nil)

	// Next render only has the 5h window (API dropped the 7d bucket).
	UpdateFromStatusLine(&CachedBucket{Pct: 22, ResetsAt: now.Add(90 * time.Minute)}, nil, nil)

	cached := loadCacheStale()
	require.NotNil(t, cached, "expected cache present")
	require.NotNil(t, cached.SevenDay, "7d window carried forward when the render omits it")
	assert.Equal(t, 35.0, cached.SevenDay.Pct, "carried-forward 7d keeps its last-known percent")
	require.NotNil(t, cached.FiveHour, "5h still present")
	assert.Equal(t, 22.0, cached.FiveHour.Pct, "fresh 5h reading wins")
}

// Once the carried 7d window's own reset time has passed, a render that
// omits it drops it — there's genuinely been no usage within the window.
func TestUpdateFromStatusLine_DropsSevenDayPastReset(t *testing.T) {
	setupTestCache(t)

	now := time.Now()
	// Seed a 7d bucket that already reset in the past.
	UpdateFromStatusLine(
		&CachedBucket{Pct: 20, ResetsAt: now.Add(time.Hour)},
		&CachedBucket{Pct: 35, ResetsAt: now.Add(-time.Minute)},
		nil,
	)

	// Next render omits the 7d bucket.
	UpdateFromStatusLine(&CachedBucket{Pct: 22, ResetsAt: now.Add(90 * time.Minute)}, nil, nil)

	cached := loadCacheStale()
	require.NotNil(t, cached, "expected cache present")
	assert.Nil(t, cached.SevenDay, "expired 7d window dropped, not carried forward")
}
