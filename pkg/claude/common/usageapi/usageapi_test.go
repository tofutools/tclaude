package usageapi

import (
	"fmt"
	"os"
	"path/filepath"
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

// stubFuncs replaces fetchFunc, credentialCandidatesFunc, and refreshTokenFunc
// for the duration of a test. The token func supplies the single credential
// source the fetch path sees. Pass nil for refresh to use a default that always
// fails (no refresh available).
func stubFuncs(t *testing.T, token func() (string, error), fetch func(string) (*Response, error)) {
	t.Helper()
	stubFuncsWithRefresh(t, token, fetch, func() (string, error) {
		return "", fmt.Errorf("no refresh token configured in test")
	})
}

func stubFuncsWithRefresh(t *testing.T, token func() (string, error), fetch func(string) (*Response, error), refresh func() (string, error)) {
	t.Helper()
	stubCandidates(t, func() []tokenCandidate {
		tok, err := token()
		if err != nil || tok == "" {
			return nil
		}
		return []tokenCandidate{{token: tok, store: storeFile}}
	})
	origFetch := fetchFunc
	origRefresh := refreshTokenFunc
	fetchFunc = fetch
	refreshTokenFunc = refresh
	t.Cleanup(func() {
		fetchFunc = origFetch
		refreshTokenFunc = origRefresh
	})
}

// stubCandidates swaps the credential-source enumerator for the duration of a test.
func stubCandidates(t *testing.T, fn func() []tokenCandidate) {
	t.Helper()
	prev := credentialCandidatesFunc
	credentialCandidatesFunc = fn
	t.Cleanup(func() { credentialCandidatesFunc = prev })
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

func TestHasClaudeOAuth(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"valid login blob", `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r"}}`, true},
		{"mcpOAuth-only file (the regression)", `{"mcpOAuth":{"some-server":{"accessToken":"x"}}}`, false},
		{"claudeAiOauth present but empty accessToken", `{"claudeAiOauth":{"accessToken":"","refreshToken":"r"}}`, false},
		{"empty object", `{}`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasClaudeOAuth([]byte(tc.data)))
		})
	}
}

// redirectHome points os.UserHomeDir at a fresh temp dir on every platform
// (HOME on Unix, USERPROFILE on Windows) and returns it.
func redirectHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// writeCredFile writes body to $HOME/.claude/.credentials.json.
func writeCredFile(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600))
}

// stubOSCredentialReader swaps the OS secret-store reader for the duration of a test.
func stubOSCredentialReader(t *testing.T, fn func() ([]byte, credentialStore)) {
	t.Helper()
	prev := osCredentialReader
	osCredentialReader = fn
	t.Cleanup(func() { osCredentialReader = prev })
}

// Both sources contribute candidates, file first: GetAccessToken returns the
// file token (highest priority), and the keychain token is enumerated behind
// it (so the fetch path can fall through to it).
func TestCredentialCandidates_FilePriority(t *testing.T) {
	home := redirectHome(t)
	writeCredFile(t, home, `{"claudeAiOauth":{"accessToken":"file-token","refreshToken":"r"}}`)
	stubOSCredentialReader(t, func() ([]byte, credentialStore) {
		return []byte(`{"claudeAiOauth":{"accessToken":"keychain-token"}}`), storeMacKeychain
	})

	cands := credentialCandidates()
	require.Len(t, cands, 2, "both file and keychain enumerated")
	assert.Equal(t, "file-token", cands[0].token)
	assert.Equal(t, storeFile, cands[0].store)
	assert.Equal(t, "keychain-token", cands[1].token)

	tok, err := GetAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "file-token", tok)
}

// Identical tokens in the file and the keychain collapse to one candidate.
func TestCredentialCandidates_DedupesIdenticalToken(t *testing.T) {
	home := redirectHome(t)
	writeCredFile(t, home, `{"claudeAiOauth":{"accessToken":"same-token"}}`)
	stubOSCredentialReader(t, func() ([]byte, credentialStore) {
		return []byte(`{"claudeAiOauth":{"accessToken":"same-token"}}`), storeMacKeychain
	})

	cands := credentialCandidates()
	require.Len(t, cands, 1, "duplicate token de-duplicated")
	assert.Equal(t, "same-token", cands[0].token)
}

// The regression: an mcpOAuth-only ~/.claude/.credentials.json (MCP server
// tokens, no login token) contributes no candidate, so the keychain token is
// the one that gets used instead of shadowing it.
func TestCredentialCandidates_McpOnlyFileSkipped(t *testing.T) {
	home := redirectHome(t)
	writeCredFile(t, home, `{"mcpOAuth":{"some-server":{"accessToken":"mcp-token"}}}`)
	stubOSCredentialReader(t, func() ([]byte, credentialStore) {
		return []byte(`{"claudeAiOauth":{"accessToken":"keychain-token","refreshToken":"r"}}`), storeMacKeychain
	})

	cands := credentialCandidates()
	require.Len(t, cands, 1, "mcpOAuth-only file contributes no candidate")
	assert.Equal(t, "keychain-token", cands[0].token)

	tok, err := GetAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "keychain-token", tok, "must use the keychain login token, not the mcpOAuth file")
}

// With no credentials file at all, the OS secret store is the sole source.
func TestCredentialCandidates_NoFile_UsesOSStore(t *testing.T) {
	redirectHome(t) // empty home, no .credentials.json
	stubOSCredentialReader(t, func() ([]byte, credentialStore) {
		return []byte(`{"claudeAiOauth":{"accessToken":"keychain-token"}}`), storeMacKeychain
	})

	tok, err := GetAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "keychain-token", tok)
}

// When neither the file nor the OS store yields a login token, the caller
// gets the honest "no token" error rather than a silent wrong source.
func TestCredentialCandidates_NoSources_Errors(t *testing.T) {
	home := redirectHome(t)
	writeCredFile(t, home, `{"mcpOAuth":{"some-server":{"accessToken":"mcp-token"}}}`)
	stubOSCredentialReader(t, func() ([]byte, credentialStore) { return nil, "" })

	require.Empty(t, credentialCandidates())
	_, err := GetAccessToken()
	require.Error(t, err)
}

// The core of the user's requirement: a stale-but-syntactically-valid file
// token (rejected 401 by the API) must not trap us — the fetch path falls
// through to the next source (the fresh keychain token) and succeeds.
func TestFetchUsage_FallsThroughOnAuthError(t *testing.T) {
	setupTestCache(t)
	stubCandidates(t, func() []tokenCandidate {
		return []tokenCandidate{
			{token: "stale-file", store: storeFile},
			{token: "fresh-keychain", store: storeMacKeychain},
		}
	})

	var fetchCount atomic.Int32
	origFetch := fetchFunc
	fetchFunc = func(token string) (*Response, error) {
		fetchCount.Add(1)
		if token == "stale-file" {
			return nil, &AuthError{Status: 401, Body: "expired"}
		}
		return okFetch(63.0)(token)
	}
	t.Cleanup(func() { fetchFunc = origFetch })

	resp, err := FetchUsage()
	require.NoError(t, err)
	require.NotNil(t, resp.FiveHour)
	assert.Equal(t, 63.0, resp.FiveHour.Utilization)
	assert.Equal(t, int32(2), fetchCount.Load(), "tried the stale token, then fell through to the keychain")
}

// A 429 is NOT a per-source auth problem: it must not trigger trying the next
// source (which is the same account and would just burn rate-limit budget).
func TestFetchUsage_NoFallthroughOn429(t *testing.T) {
	setupTestCache(t)
	stubCandidates(t, func() []tokenCandidate {
		return []tokenCandidate{
			{token: "first", store: storeFile},
			{token: "second", store: storeMacKeychain},
		}
	})

	var fetchCount atomic.Int32
	origFetch := fetchFunc
	fetchFunc = func(token string) (*Response, error) {
		fetchCount.Add(1)
		return rateLimitFetch(token)
	}
	t.Cleanup(func() { fetchFunc = origFetch })

	_, err := FetchUsage()
	require.Error(t, err)
	assert.Equal(t, int32(1), fetchCount.Load(), "429 stops at the first source — no per-source fallthrough")
}

// When every source's token is rejected, the last auth error is returned.
func TestFetchUsage_AllSourcesAuthFail(t *testing.T) {
	setupTestCache(t)
	stubCandidates(t, func() []tokenCandidate {
		return []tokenCandidate{
			{token: "a", store: storeFile},
			{token: "b", store: storeMacKeychain},
		}
	})

	var fetchCount atomic.Int32
	origFetch := fetchFunc
	fetchFunc = func(token string) (*Response, error) {
		fetchCount.Add(1)
		return nil, &AuthError{Status: 401, Body: "nope"}
	}
	t.Cleanup(func() { fetchFunc = origFetch })

	_, err := FetchUsage()
	require.Error(t, err)
	var authErr *AuthError
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, int32(2), fetchCount.Load(), "tried every source before giving up")
}

func TestCarryForwardWindow(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	future := &CachedBucket{Pct: 40, ResetsAt: now.Add(3 * 24 * time.Hour)}
	fresh := &CachedBucket{Pct: 12, ResetsAt: now.Add(2 * time.Hour)}
	liveZero := &CachedBucket{Pct: 0, ResetsAt: now.Add(time.Hour)}

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
		// A 0% window whose reset is still in the future is the current
		// period with no spend yet — real data, carried forward so it keeps
		// its remaining-time hint (matches the dashboard's liveUsageWindow).
		{"omitted + live zero-usage prev → carried forward", nil, liveZero, liveZero},
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

// The clobber guard: when a fresh reading carries a real percent but no
// usable reset (the Anthropic weekly bucket often drops resets_at), it must
// NOT erase a previously-cached reset that is still in the future — that
// would blank the bar's remaining-time hint and lose the window's real
// expiry. The fresh percent wins; the prior future reset is grafted on.
func TestCarryForwardWindow_KeepsFutureResetOverResetlessFresh(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	prev := &CachedBucket{Pct: 35, ResetsAt: now.Add(4 * 24 * time.Hour)}

	// Fresh: real percent, no reset at all.
	freshNoReset := &CachedBucket{Pct: 36}
	got := carryForwardWindow(freshNoReset, prev, now)
	require.NotNil(t, got)
	assert.Equal(t, 36.0, got.Pct, "fresh percent wins")
	assert.Equal(t, prev.ResetsAt, got.ResetsAt, "prior future reset grafted on")

	// Fresh: real percent, an already-elapsed reset — also downgraded, so
	// the same graft applies.
	freshPastReset := &CachedBucket{Pct: 37, ResetsAt: now.Add(-time.Hour)}
	got = carryForwardWindow(freshPastReset, prev, now)
	require.NotNil(t, got)
	assert.Equal(t, 37.0, got.Pct, "fresh percent wins")
	assert.Equal(t, prev.ResetsAt, got.ResetsAt, "prior future reset grafted over the elapsed one")

	// But when the prior reset is ALSO gone, there's nothing better to
	// borrow — the fresh bucket is returned untouched.
	got = carryForwardWindow(freshNoReset, &CachedBucket{Pct: 30, ResetsAt: now.Add(-time.Hour)}, now)
	require.Same(t, freshNoReset, got, "no future prior reset to borrow → fresh returned as-is")
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
