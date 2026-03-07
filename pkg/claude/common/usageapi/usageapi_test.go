package usageapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	stale.LastAttemptAt = time.Now().Add(-CacheTTL() - 30*time.Second)
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
	stale.LastAttemptAt = time.Now().Add(-CacheTTL() - 30*time.Second)
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
	stale.LastAttemptAt = time.Now().Add(-CacheTTL() - 30*time.Second)
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
	cached.LastAttemptAt = time.Now().Add(-CacheTTL() - 30*time.Second)
	saveCache(cached)

	result = loadCache()
	if result != nil {
		t.Fatal("expected expired cache, got non-nil")
	}
}

func TestStampLastAttempt_NoCacheFile_CreatesMinimalEntry(t *testing.T) {
	setupTestCache(t)

	stampLastAttempt()

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
	stale.LastAttemptAt = time.Now().Add(-CacheTTL() - 30*time.Second)
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
	stale.LastAttemptAt = time.Now().Add(-CacheTTL() - 30*time.Second)
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

// sampleCredentialsJSON returns a valid credentials JSON blob for testing.
func sampleCredentialsJSON() []byte {
	data, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "test-access-token",
			"refreshToken": "test-refresh-token",
			"expiresAt":    time.Now().Add(time.Hour).UnixMilli(),
		},
	})
	return data
}

// setupTclaudeHome sets HOME to a temp dir and returns paths for the
// tclaude credentials file and the Claude credentials file.
func setupTclaudeHome(t *testing.T) (tclaudeCreds string, claudeCreds string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	tclaudeDir := filepath.Join(home, ".tclaude")
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(tclaudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(tclaudeDir, "api-credentials.json"),
		filepath.Join(claudeDir, ".credentials.json")
}

func TestReadCredentials_PrefersOwnFile(t *testing.T) {
	tclaudeCreds, claudeCreds := setupTclaudeHome(t)

	// Write different tokens to each file
	tclaudeData, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "tclaude-token",
			"refreshToken": "tclaude-refresh",
		},
	})
	claudeData, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "claude-token",
			"refreshToken": "claude-refresh",
		},
	})
	os.WriteFile(tclaudeCreds, tclaudeData, 0600)
	os.WriteFile(claudeCreds, claudeData, 0600)

	result, err := readCredentialsJSON()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.store != storeTclaude {
		t.Errorf("expected store %q, got %q", storeTclaude, result.store)
	}

	// Verify it's the tclaude token
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	json.Unmarshal(result.data, &creds)
	if creds.ClaudeAiOauth.AccessToken != "tclaude-token" {
		t.Errorf("expected tclaude-token, got %s", creds.ClaudeAiOauth.AccessToken)
	}
}

func TestReadCredentials_FallsBackToClaude(t *testing.T) {
	_, claudeCreds := setupTclaudeHome(t)

	// Only write Claude's credentials (no tclaude file)
	claudeData, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "claude-token",
			"refreshToken": "claude-refresh",
		},
	})
	os.WriteFile(claudeCreds, claudeData, 0600)

	result, err := readCredentialsJSON()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.store != storeFile {
		t.Errorf("expected store %q, got %q", storeFile, result.store)
	}
}

func TestReadCredentials_NeitherFileExists(t *testing.T) {
	setupTclaudeHome(t) // creates dirs but no credential files

	_, err := readCredentialsJSON()
	if err == nil {
		t.Fatal("expected error when no credentials exist")
	}
}

func TestUsingOwnCredentials(t *testing.T) {
	tclaudeCreds, _ := setupTclaudeHome(t)

	// No tclaude file yet
	if usingOwnCredentials() {
		t.Error("expected false when tclaude file does not exist")
	}

	// Create the file
	os.WriteFile(tclaudeCreds, sampleCredentialsJSON(), 0600)

	if !usingOwnCredentials() {
		t.Error("expected true when tclaude file exists")
	}
}

func TestCanRefreshToken_OwnCredentials(t *testing.T) {
	tclaudeCreds, _ := setupTclaudeHome(t)
	setupTestCache(t)

	// Without own file and without env var — should not refresh
	t.Setenv("TCLAUDE_DEBUG_REFRESH", "")
	if canRefreshToken() {
		t.Error("expected false without own credentials or env var")
	}

	// With own file — should refresh
	os.WriteFile(tclaudeCreds, sampleCredentialsJSON(), 0600)
	if !canRefreshToken() {
		t.Error("expected true with own credentials file")
	}
}

func TestCanRefreshToken_EnvOverride(t *testing.T) {
	setupTclaudeHome(t) // no tclaude creds file
	t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")

	if !canRefreshToken() {
		t.Error("expected true with TCLAUDE_DEBUG_REFRESH=1")
	}
}

func TestWriteCredentials_StoreTclaude(t *testing.T) {
	tclaudeCreds, _ := setupTclaudeHome(t)

	data := sampleCredentialsJSON()
	if err := writeCredentials(storeTclaude, data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written, err := os.ReadFile(tclaudeCreds)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(written) != string(data) {
		t.Errorf("written data mismatch")
	}
}

func TestWriteCredentials_StoreFile_DoesNotTouchTclaude(t *testing.T) {
	tclaudeCreds, claudeCreds := setupTclaudeHome(t)

	data := sampleCredentialsJSON()
	if err := writeCredentials(storeFile, data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Claude's file should be written
	if _, err := os.ReadFile(claudeCreds); err != nil {
		t.Fatalf("expected Claude credentials file to be written: %v", err)
	}

	// Tclaude's file should NOT exist
	if _, err := os.ReadFile(tclaudeCreds); err == nil {
		t.Error("tclaude file should not be created by storeFile write")
	}
}

func TestGetCached_429RefreshesWithOwnCredentials(t *testing.T) {
	tclaudeCreds, _ := setupTclaudeHome(t)
	setupTestCache(t)
	// Ensure TCLAUDE_DEBUG_REFRESH is not set — refresh should work via own creds
	t.Setenv("TCLAUDE_DEBUG_REFRESH", "")

	os.WriteFile(tclaudeCreds, sampleCredentialsJSON(), 0600)

	var fetchCount atomic.Int32
	var refreshCount atomic.Int32

	stubFuncsWithRefresh(t, okToken,
		func(token string) (*Response, error) {
			n := fetchCount.Add(1)
			if n == 1 {
				return rateLimitFetch(token)
			}
			return okFetch(88.0)(token)
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
	if result.FiveHour == nil || result.FiveHour.Pct != 88.0 {
		t.Fatalf("expected 88%%, got %+v", result.FiveHour)
	}
	if refreshCount.Load() != 1 {
		t.Errorf("expected 1 refresh, got %d", refreshCount.Load())
	}
}

func TestGetCached_429DoesNotRefreshWithoutOwnCredentials(t *testing.T) {
	setupTclaudeHome(t) // no tclaude creds file
	setupTestCache(t)
	t.Setenv("TCLAUDE_DEBUG_REFRESH", "")

	var refreshCount atomic.Int32

	stubFuncsWithRefresh(t, okToken, rateLimitFetch,
		func() (string, error) {
			refreshCount.Add(1)
			return "refreshed-token", nil
		},
	)

	_, err := GetCached()
	if err == nil {
		t.Fatal("expected error on 429 without own credentials")
	}
	if refreshCount.Load() != 0 {
		t.Errorf("expected 0 refreshes without own credentials, got %d", refreshCount.Load())
	}
}
