// Package usageapi provides access to the Anthropic OAuth usage API
// for fetching subscription usage limits.
package usageapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	usageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	tokenEndpoint = "https://console.anthropic.com/v1/oauth/token"
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	cacheTTL      = 5 * time.Minute
)

// fetchFunc, getTokenFunc, and refreshTokenFunc are swappable for testing.
var (
	fetchFunc        = Fetch
	getTokenFunc     = GetAccessToken
	refreshTokenFunc = RefreshAccessToken
)

// RateLimitError is returned when the API responds with HTTP 429.
type RateLimitError struct {
	Body string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (429): %s", e.Body)
}

// credentialStore identifies where credentials were loaded from.
type credentialStore string

const (
	storeFile         credentialStore = "file"
	storeMacKeychain  credentialStore = "macOS keychain"
	storeLinuxKeyring credentialStore = "Linux keyring"
)

// credentialResult bundles the raw JSON with where it came from.
type credentialResult struct {
	data  []byte
	store credentialStore
}

// Response represents the API response from the usage endpoint.
// All buckets are optional since they are only present on subscription plans.
type Response struct {
	FiveHour       *Bucket     `json:"five_hour"`
	SevenDay       *Bucket     `json:"seven_day"`
	SevenDaySonnet *Bucket     `json:"seven_day_sonnet"`
	ExtraUsage     *ExtraUsage `json:"extra_usage"`
}

// Bucket represents a single usage time bucket
type Bucket struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// ExtraUsage represents the extra usage / overuse allowance
type ExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

// CachedBucket stores utilization percentage and reset time for a single bucket
type CachedBucket struct {
	Pct      float64   `json:"pct"`
	ResetsAt time.Time `json:"resets_at"`
}

// CachedUsage is persisted to disk for caching. All buckets are optional
// since they are only available on subscription plans.
type CachedUsage struct {
	FiveHour       *CachedBucket `json:"five_hour,omitempty"`
	SevenDay       *CachedBucket `json:"seven_day,omitempty"`
	SevenDaySonnet *CachedBucket `json:"seven_day_sonnet,omitempty"` // only for premium/max
	ExtraUsage     *ExtraUsage   `json:"extra_usage,omitempty"`
	FetchedAt      time.Time     `json:"fetched_at"`
	LastAttemptAt  time.Time     `json:"last_attempt_at,omitempty"`
	LastError      string        `json:"last_error,omitempty"`
}

// loadCache returns cached usage if still fresh (within TTL).
func loadCache() *CachedUsage {
	return loadCacheWithTTL(cacheTTL)
}

// loadCacheStale returns cached usage regardless of age, or nil if missing/corrupt.
func loadCacheStale() *CachedUsage {
	return loadCacheWithTTL(0)
}

func loadCacheWithTTL(ttl time.Duration) *CachedUsage {
	row, err := db.LoadUsageCache()
	if err != nil || row == nil {
		return nil
	}

	var cached CachedUsage
	if err := json.Unmarshal(row.Data, &cached); err != nil {
		return nil
	}

	if ttl > 0 && time.Since(cached.LastAttemptAt) > ttl {
		return nil
	}

	return &cached
}

// saveCache persists usage data to SQLite.
func saveCache(usage *CachedUsage) {
	data, err := json.Marshal(usage)
	if err != nil {
		return
	}
	if err := db.SaveUsageCache(data, usage.FetchedAt, usage.LastAttemptAt); err != nil {
		slog.Warn("failed to save usage cache", "error", err)
	}
}

func credentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// currentUsername returns the current OS username, or empty string.
func currentUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// ReadCredentialsForTest exposes readCredentialsJSON for integration testing.
func ReadCredentialsForTest() (data []byte, store string, err error) {
	result, err := readCredentialsJSON()
	if err != nil {
		return nil, "", err
	}
	return result.data, string(result.store), nil
}

// readCredentialsJSON returns the raw credentials JSON and where it was found,
// trying the file first, then falling back to the OS keychain/keyring.
func readCredentialsJSON() (*credentialResult, error) {
	// Try the credentials file first
	path := credentialsPath()
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			slog.Debug("credentials read from file", "path", path)
			return &credentialResult{data: data, store: storeFile}, nil
		}
	}

	// On macOS, fall back to the keychain
	if runtime.GOOS == "darwin" {
		if data := readMacKeychain(); data != nil {
			slog.Debug("credentials read from macOS keychain")
			return &credentialResult{data: data, store: storeMacKeychain}, nil
		}
	}

	// On Linux, fall back to secret-tool (libsecret / GNOME Keyring)
	if runtime.GOOS == "linux" {
		if data := readLinuxKeyring(); data != nil {
			slog.Debug("credentials read from Linux keyring")
			return &credentialResult{data: data, store: storeLinuxKeyring}, nil
		}
	}

	return nil, fmt.Errorf("cannot read credentials from file or keychain/keyring")
}

// readMacKeychain reads credentials from macOS Keychain.
// Tries with username first (Claude Code's convention), then without.
func readMacKeychain() []byte {
	if username := currentUsername(); username != "" {
		out, err := exec.Command("security", "find-generic-password",
			"-s", "Claude Code-credentials", "-a", username, "-w").Output()
		if err == nil {
			return []byte(strings.TrimSpace(string(out)))
		}
	}
	// Fallback: no account filter
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err == nil {
		return []byte(strings.TrimSpace(string(out)))
	}
	return nil
}

// readLinuxKeyring reads credentials from Linux keyring via secret-tool.
func readLinuxKeyring() []byte {
	if username := currentUsername(); username != "" {
		out, err := exec.Command("secret-tool", "lookup",
			"service", "Claude Code-credentials", "account", username).Output()
		if err == nil && len(out) > 0 {
			return []byte(strings.TrimSpace(string(out)))
		}
	}
	// Fallback: without account
	out, err := exec.Command("secret-tool", "lookup",
		"service", "Claude Code-credentials").Output()
	if err == nil && len(out) > 0 {
		return []byte(strings.TrimSpace(string(out)))
	}
	return nil
}

// GetAccessToken reads the OAuth access token from Claude credentials
func GetAccessToken() (string, error) {
	result, err := readCredentialsJSON()
	if err != nil {
		return "", err
	}

	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(result.data, &creds); err != nil {
		return "", fmt.Errorf("cannot parse credentials: %w", err)
	}

	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no access token found in credentials")
	}

	return creds.ClaudeAiOauth.AccessToken, nil
}

// RefreshAccessToken uses the refresh token to obtain a new access token,
// writes the updated credentials back, and returns the new access token.
// This resets the per-token rate limit on the usage API.
func RefreshAccessToken() (string, error) {
	result, err := readCredentialsJSON()
	if err != nil {
		return "", fmt.Errorf("cannot read credentials for refresh: %w", err)
	}

	// Parse only what we need for the refresh
	var creds struct {
		ClaudeAiOauth struct {
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(result.data, &creds); err != nil {
		return "", fmt.Errorf("cannot parse credentials for refresh: %w", err)
	}
	if creds.ClaudeAiOauth.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token found in credentials")
	}

	slog.Info("refreshing OAuth access token to reset rate limit",
		"store", string(result.store))

	// Call the token endpoint
	reqBody, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": creds.ClaudeAiOauth.RefreshToken,
		"client_id":     oauthClientID,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(tokenEndpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		slog.Warn("token refresh request failed", "error", err)
		return "", fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("token refresh response read failed: %w", err)
	}

	if resp.StatusCode != 200 {
		slog.Warn("token refresh returned non-200", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("token refresh returned %d: %s", resp.StatusCode, string(body))
	}

	// Parse the refresh response for the new tokens
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("cannot parse token refresh response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("token refresh response missing access_token")
	}

	slog.Info("OAuth token refreshed successfully",
		"has_new_refresh_token", tokenResp.RefreshToken != "",
		"expires_in_seconds", tokenResp.ExpiresIn,
		"store", string(result.store))

	// Update the credentials blob preserving all other fields
	if err := updateCredentials(result, tokenResp.AccessToken, tokenResp.RefreshToken, tokenResp.ExpiresIn); err != nil {
		slog.Warn("failed to save refreshed credentials", "error", err,
			"store", string(result.store))
		// Still return the new token — it works even if we can't persist it
	}

	return tokenResp.AccessToken, nil
}

// updateCredentials patches the credential blob with new token values and writes it back.
// Preserves all existing fields by doing a partial update on the raw JSON.
func updateCredentials(cred *credentialResult, accessToken, refreshToken string, expiresIn int64) error {
	// Deserialize into a generic map to preserve all fields
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(cred.data, &blob); err != nil {
		return fmt.Errorf("cannot parse credentials blob: %w", err)
	}

	// Parse the claudeAiOauth sub-object
	oauthRaw, ok := blob["claudeAiOauth"]
	if !ok {
		return fmt.Errorf("credentials missing claudeAiOauth")
	}
	var oauth map[string]json.RawMessage
	if err := json.Unmarshal(oauthRaw, &oauth); err != nil {
		return fmt.Errorf("cannot parse claudeAiOauth: %w", err)
	}

	// Update only the token fields
	oauth["accessToken"], _ = json.Marshal(accessToken)
	if refreshToken != "" {
		oauth["refreshToken"], _ = json.Marshal(refreshToken)
	}
	if expiresIn > 0 {
		expiresAt := time.Now().UnixMilli() + expiresIn*1000
		oauth["expiresAt"], _ = json.Marshal(expiresAt)
	}

	// Reassemble
	blob["claudeAiOauth"], _ = json.Marshal(oauth)
	updated, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("cannot serialize updated credentials: %w", err)
	}

	return writeCredentials(cred.store, updated)
}

// writeCredentials writes credentials back to the specified store.
func writeCredentials(store credentialStore, data []byte) error {
	switch store {
	case storeFile:
		path := credentialsPath()
		if path == "" {
			return fmt.Errorf("credentials file path unavailable")
		}
		slog.Info("writing refreshed credentials to file", "path", path)
		return os.WriteFile(path, data, 0600)

	case storeMacKeychain:
		username := currentUsername()
		slog.Info("writing refreshed credentials to macOS keychain", "username", username)
		cmd := exec.Command("security", "add-generic-password",
			"-s", "Claude Code-credentials",
			"-a", username,
			"-w", string(data),
			"-U", // update if exists
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("keychain write failed: %w (output: %s)", err, string(out))
		}
		return nil

	case storeLinuxKeyring:
		username := currentUsername()
		slog.Info("writing refreshed credentials to Linux keyring", "username", username)
		cmd := exec.Command("secret-tool", "store",
			"--label", "Claude Code-credentials",
			"service", "Claude Code-credentials",
			"account", username)
		cmd.Stdin = bytes.NewReader(data)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("keyring write failed: %w (output: %s)", err, string(out))
		}
		return nil

	default:
		return fmt.Errorf("unknown credential store: %s", store)
	}
}

// FetchRawWithRetry calls the usage API. On 429 it attempts a token refresh
// only when TCLAUDE_DEBUG_REFRESH=1 is set (disabled by default because
// refreshing from tclaude invalidates Claude Code's in-memory refresh token).
func FetchRawWithRetry(token string) ([]byte, error) {
	body, err := FetchRaw(token)
	if err == nil {
		return body, nil
	}

	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		return nil, err
	}

	if os.Getenv("TCLAUDE_DEBUG_REFRESH") != "1" {
		return nil, err
	}

	slog.Info("FetchRawWithRetry: got 429, attempting token refresh (TCLAUDE_DEBUG_REFRESH=1)")
	newToken, refreshErr := refreshTokenFunc()
	if refreshErr != nil {
		slog.Warn("FetchRawWithRetry: token refresh failed", "error", refreshErr)
		return nil, err
	}

	slog.Info("FetchRawWithRetry: retrying with new token")
	body, retryErr := FetchRaw(newToken)
	if retryErr != nil {
		slog.Warn("FetchRawWithRetry: retry failed after token refresh", "error", retryErr)
		return nil, retryErr
	}

	slog.Info("FetchRawWithRetry: succeeded after token refresh")
	return body, nil
}

// FetchRaw calls the Anthropic usage API and returns the raw JSON response body.
func FetchRaw(token string) ([]byte, error) {
	req, err := http.NewRequest("GET", usageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	slog.Debug("fetching usage data from API")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("usage API request failed", "error", err)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("usage API response read failed", "error", err)
		return nil, err
	}

	if resp.StatusCode == 429 {
		slog.Warn("usage API rate limited (429)", "body", string(body))
		return nil, &RateLimitError{Body: string(body)}
	}

	if resp.StatusCode != 200 {
		slog.Warn("usage API returned non-200", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	slog.Debug("usage API fetch succeeded", "status", resp.StatusCode)
	return body, nil
}

// FetchWithRetry calls the usage API and on 429 refreshes the token and retries once.
func FetchWithRetry(token string) (*Response, error) {
	return fetchWithRateLimitRetry(token)
}

// Fetch calls the Anthropic usage API and returns the parsed response.
func Fetch(token string) (*Response, error) {
	body, err := FetchRaw(token)
	if err != nil {
		return nil, err
	}

	var usage Response
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("cannot parse usage response: %w", err)
	}

	return &usage, nil
}

// parseBucket converts an API Bucket to a CachedBucket
func parseBucket(b Bucket) CachedBucket {
	cb := CachedBucket{Pct: b.Utilization}
	if b.ResetsAt != "" {
		t, err := time.Parse(time.RFC3339, b.ResetsAt)
		if err == nil {
			cb.ResetsAt = t
		}
	}
	return cb
}

// stampLastAttempt updates LastAttemptAt on the cache file so that
// subsequent calls respect the cacheTTL backoff even after a failed fetch.
// Creates a minimal cache entry if none exists. Records the error message.
func stampLastAttempt(err error) {
	cached := loadCacheStale()
	if cached == nil {
		cached = &CachedUsage{}
	}
	cached.LastAttemptAt = time.Now()
	if err != nil {
		cached.LastError = err.Error()
	}
	saveCache(cached)
}

// fetchWithRateLimitRetry fetches usage data. On 429 it attempts a token
// refresh only when TCLAUDE_DEBUG_REFRESH=1 is set (disabled by default
// because refreshing from tclaude invalidates Claude Code's in-memory
// refresh token).
func fetchWithRateLimitRetry(token string) (*Response, error) {
	resp, err := fetchFunc(token)
	if err == nil {
		return resp, nil
	}

	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		return nil, err
	}

	if os.Getenv("TCLAUDE_DEBUG_REFRESH") != "1" {
		return nil, err
	}

	slog.Info("got 429, attempting token refresh (TCLAUDE_DEBUG_REFRESH=1)")
	newToken, refreshErr := refreshTokenFunc()
	if refreshErr != nil {
		slog.Warn("token refresh failed after 429, giving up", "error", refreshErr)
		return nil, err // return original 429 error
	}

	slog.Info("token refreshed, retrying usage fetch with new token")
	resp, retryErr := fetchFunc(newToken)
	if retryErr != nil {
		slog.Warn("usage fetch failed even after token refresh", "error", retryErr)
		return nil, retryErr
	}

	slog.Info("usage fetch succeeded after token refresh")
	return resp, nil
}

// buildCachedUsage converts an API Response to a CachedUsage.
func buildCachedUsage(resp *Response) *CachedUsage {
	now := time.Now()
	cached := &CachedUsage{
		FetchedAt:     now,
		LastAttemptAt: now,
	}
	if resp.FiveHour != nil {
		b := parseBucket(*resp.FiveHour)
		cached.FiveHour = &b
	}
	if resp.SevenDay != nil {
		b := parseBucket(*resp.SevenDay)
		cached.SevenDay = &b
	}
	if resp.SevenDaySonnet != nil {
		b := parseBucket(*resp.SevenDaySonnet)
		cached.SevenDaySonnet = &b
	}
	if resp.ExtraUsage != nil {
		cached.ExtraUsage = resp.ExtraUsage
	}
	return cached
}

// RefreshCache updates the cache if stale. Called from hooks when the user is
// likely looking at the status bar. Uses atomic SQLite claim to prevent
// concurrent hook processes from all hitting the API simultaneously.
func RefreshCache() {
	// Atomic check-and-claim: only one process proceeds per TTL window.
	// If we crash after claiming, the TTL expires naturally (no stuck locks).
	claimed, err := db.TryClaimUsageFetch(cacheTTL)
	if err != nil {
		slog.Warn("RefreshCache: failed to check cache", "error", err)
		return
	}
	if !claimed {
		return // still fresh or another process claimed it
	}

	token, err := getTokenFunc()
	if err != nil {
		slog.Warn("RefreshCache: failed to get access token", "error", err)
		stampLastAttempt(err)
		return
	}
	resp, err := fetchWithRateLimitRetry(token)
	if err != nil {
		slog.Warn("RefreshCache: failed to fetch usage data", "error", err)
		stampLastAttempt(err)
		return
	}
	saveCache(buildCachedUsage(resp))
}

// UpdateFromStatusLine updates the usage cache with rate limit data received
// from Claude Code's statusline input. This keeps the cache fresh without
// needing an API call, so other consumers (e.g. new sessions before their
// first API response) see up-to-date data.
func UpdateFromStatusLine(fiveHour, sevenDay, sevenDaySonnet *CachedBucket) {
	now := time.Now()
	cached := &CachedUsage{
		FiveHour:       fiveHour,
		SevenDay:       sevenDay,
		SevenDaySonnet: sevenDaySonnet,
		FetchedAt:      now,
		LastAttemptAt:  now,
	}
	// Preserve extra usage data from any existing cache entry
	if existing := loadCacheStale(); existing != nil {
		cached.ExtraUsage = existing.ExtraUsage
	}
	saveCache(cached)
}

// GetCached returns usage percentages, using a file cache (5 min TTL) to
// avoid hammering the API. On fetch errors, returns stale cached data if available.
func GetCached() (*CachedUsage, error) {
	if cached := loadCache(); cached != nil {
		return cached, nil
	}

	token, err := getTokenFunc()
	if err != nil {
		stampLastAttempt(err)
		if stale := loadCacheStale(); stale != nil && !stale.FetchedAt.IsZero() {
			return stale, fmt.Errorf("using stale cache: %w", err)
		}
		slog.Warn("no usage data available", "error", err)
		return nil, err
	}

	resp, err := fetchWithRateLimitRetry(token)
	if err != nil {
		stampLastAttempt(err)
		if stale := loadCacheStale(); stale != nil && !stale.FetchedAt.IsZero() {
			return stale, fmt.Errorf("using stale cache: %w", err)
		}
		slog.Warn("no usage data available", "error", err)
		return nil, err
	}

	cached := buildCachedUsage(resp)
	saveCache(cached)
	return cached, nil
}
