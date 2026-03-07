package usageapi

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// setupTestCache points the cache at a temp dir and returns a cleanup function.
func setupTestCache(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FiveHour == nil || result.FiveHour.Pct != 42.0 {
		t.Fatalf("expected 42%%, got %+v", result.FiveHour)
	}

	// Second call within TTL should use cache, not fetch again
	result2, err := GetCached()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.FiveHour == nil || result2.FiveHour.Pct != 42.0 {
		t.Fatalf("expected 42%%, got %+v", result2.FiveHour)
	}

	if fetchCount.Load() != 1 {
		t.Errorf("expected 1 fetch call, got %d", fetchCount.Load())
	}
}

func TestGetCached_ErrorReturnsStaleCacheAndError(t *testing.T) {
	setupTestCache(t)

	// Seed the cache with a successful fetch
	stubFuncs(t, okToken, okFetch(50.0))
	if _, err := GetCached(); err != nil {
		t.Fatalf("seed fetch failed: %v", err)
	}

	// Expire the cache by backdating LastAttemptAt
	stale := loadCacheStale()
	if stale == nil {
		t.Fatal("expected stale cache")
	}
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	// Now make fetch fail
	stubFuncs(t, okToken, failFetch)
	result, err := GetCached()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result == nil {
		t.Fatal("expected stale data, got nil")
	}
	if result.FiveHour == nil || result.FiveHour.Pct != 50.0 {
		t.Fatalf("expected stale 50%%, got %+v", result.FiveHour)
	}
}

func TestGetCached_BackoffAfterError(t *testing.T) {
	setupTestCache(t)

	// Seed cache
	stubFuncs(t, okToken, okFetch(25.0))
	if _, err := GetCached(); err != nil {
		t.Fatalf("seed fetch failed: %v", err)
	}

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

	GetCached()

	// Second call should NOT fetch — backoff is active
	GetCached()

	if fetchCount.Load() != 1 {
		t.Errorf("expected 1 fetch during backoff, got %d", fetchCount.Load())
	}
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

	if fetchCount.Load() != 1 {
		t.Errorf("expected 1 fetch during backoff, got %d", fetchCount.Load())
	}
}

func TestGetCached_NoStaleCache_ReturnsNilAndError(t *testing.T) {
	setupTestCache(t)

	stubFuncs(t, okToken, failFetch)

	result, err := GetCached()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// No prior successful fetch, so no usage data to return
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
}

func TestLoadCacheStale_NoCacheFile_ReturnsNil(t *testing.T) {
	setupTestCache(t)
	// No cache file exists — loadCacheStale should return nil
	if stale := loadCacheStale(); stale != nil {
		t.Fatalf("expected nil, got %+v", stale)
	}
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
	if result == nil {
		t.Fatal("expected fresh cache based on LastAttemptAt, got nil")
	}

	// Now backdate LastAttemptAt too
	cached.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(cached)

	result = loadCache()
	if result != nil {
		t.Fatal("expected expired cache, got non-nil")
	}
}

func TestStampLastAttempt_NoCacheFile_CreatesMinimalEntry(t *testing.T) {
	setupTestCache(t)

	stampLastAttempt(fmt.Errorf("test error"))

	// Should create a minimal cache entry with just LastAttemptAt
	cached := loadCacheStale()
	if cached == nil {
		t.Fatal("expected minimal cache entry, got nil")
	}
	if cached.FiveHour != nil || cached.SevenDay != nil {
		t.Fatal("expected no usage data in minimal entry")
	}
	if time.Since(cached.LastAttemptAt) > time.Second {
		t.Fatal("expected recent LastAttemptAt")
	}
}

func TestGetCached_BackoffEvenWithoutStaleCache(t *testing.T) {
	setupTestCache(t)

	var fetchCount atomic.Int32
	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return failFetch(token)
	})

	// First call fails, no stale cache
	GetCached()

	// Second call should NOT fetch — backoff is active even without stale data
	GetCached()

	if fetchCount.Load() != 1 {
		t.Errorf("expected 1 fetch during backoff, got %d", fetchCount.Load())
	}
}

func TestGetCached_429ReturnsErrorByDefault(t *testing.T) {
	setupTestCache(t)

	// Seed cache so we get stale fallback
	stubFuncs(t, okToken, okFetch(42.0))
	if _, err := GetCached(); err != nil {
		t.Fatalf("seed fetch failed: %v", err)
	}
	stale := loadCacheStale()
	stale.LastAttemptAt = time.Now().Add(-cacheTTL - 30*time.Second)
	saveCache(stale)

	stubFuncs(t, okToken, rateLimitFetch)

	result, err := GetCached()
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if result == nil || result.FiveHour == nil || result.FiveHour.Pct != 42.0 {
		t.Fatalf("expected stale 42%%, got %+v", result)
	}
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
	if err != nil {
		t.Fatalf("expected success after refresh, got error: %v", err)
	}
	if result.FiveHour == nil || result.FiveHour.Pct != 77.0 {
		t.Fatalf("expected 77%%, got %+v", result.FiveHour)
	}
	if fetchCount.Load() != 2 {
		t.Errorf("expected 2 fetches (original + retry), got %d", fetchCount.Load())
	}
	if refreshCount.Load() != 1 {
		t.Errorf("expected 1 refresh, got %d", refreshCount.Load())
	}
}

func TestGetCached_429RefreshFailsFallsBackToStale(t *testing.T) {
	setupTestCache(t)

	// Seed cache
	stubFuncs(t, okToken, okFetch(33.0))
	if _, err := GetCached(); err != nil {
		t.Fatalf("seed fetch failed: %v", err)
	}

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
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result == nil {
		t.Fatal("expected stale data, got nil")
	}
	if result.FiveHour == nil || result.FiveHour.Pct != 33.0 {
		t.Fatalf("expected stale 33%%, got %+v", result.FiveHour)
	}
}

func TestRefreshCache_429DoesNotRetryByDefault(t *testing.T) {
	setupTestCache(t)

	var fetchCount atomic.Int32

	stubFuncs(t, okToken, func(token string) (*Response, error) {
		fetchCount.Add(1)
		return rateLimitFetch(token)
	})

	RefreshCache()

	if fetchCount.Load() != 1 {
		t.Errorf("expected 1 fetch (no retry), got %d", fetchCount.Load())
	}
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
	if cached == nil {
		t.Fatal("expected cache to be populated after refresh+retry")
	}
	if cached.FiveHour == nil || cached.FiveHour.Pct != 55.0 {
		t.Fatalf("expected 55%%, got %+v", cached.FiveHour)
	}
	if fetchCount.Load() != 2 {
		t.Errorf("expected 2 fetches, got %d", fetchCount.Load())
	}
}
