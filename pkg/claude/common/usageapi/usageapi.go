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

	"github.com/tofutools/tclaude/pkg/common"
)

const (
	usageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	tokenEndpoint = "https://console.anthropic.com/v1/oauth/token"
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	cacheTTLShared = 5 * time.Minute
	cacheTTLOwn    = 30 * time.Second
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
	storeTclaude      credentialStore = "tclaude file"
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
}

func cachePath() string {
	cacheDir := common.CacheDir()
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "claude-usage.json")
}

func credentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// tclaudeCredentialsPath returns the path to tclaude's own credential file.
func tclaudeCredentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "api-credentials.json")
}

// usingOwnCredentials returns true if tclaude has its own credential file,
// meaning token refresh is safe (won't conflict with Claude Code).
func usingOwnCredentials() bool {
	path := tclaudeCredentialsPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
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

// readCredentialsJSON returns the raw credentials JSON and where it was found.
// It checks tclaude's own credential file (~/.tclaude/api-credentials.json)
// first. If present, tclaude uses its own token independently of Claude Code.
// Otherwise falls back to Claude's stores (file, keychain/keyring).
func readCredentialsJSON() (*credentialResult, error) {
	// Check tclaude's own credential file first
	tcPath := tclaudeCredentialsPath()
	if tcPath != "" {
		if data, err := os.ReadFile(tcPath); err == nil {
			slog.Debug("credentials read from tclaude file", "path", tcPath)
			return &credentialResult{data: data, store: storeTclaude}, nil
		}
	}

	// Fall back to Claude's credential stores
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

// writeTclaudeCredentials writes credentials to tclaude's own file atomically.
func writeTclaudeCredentials(data []byte) error {
	path := tclaudeCredentialsPath()
	if path == "" {
		return fmt.Errorf("tclaude credentials path unavailable")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create tclaude dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// TclaudeCredentialsPath returns the path to tclaude's own credential file.
func TclaudeCredentialsPath() string {
	return tclaudeCredentialsPath()
}

// ReadClaudeCredentials reads credentials from Claude Code's stores only
// (file, keychain/keyring), skipping tclaude's own file. Returns the raw
// JSON and a human-readable store name.
func ReadClaudeCredentials() (data []byte, store string, err error) {
	path := credentialsPath()
	if path != "" {
		if d, err := os.ReadFile(path); err == nil {
			return d, string(storeFile), nil
		}
	}
	if runtime.GOOS == "darwin" {
		if d := readMacKeychain(); d != nil {
			return d, string(storeMacKeychain), nil
		}
	}
	if runtime.GOOS == "linux" {
		if d := readLinuxKeyring(); d != nil {
			return d, string(storeLinuxKeyring), nil
		}
	}
	return nil, "", fmt.Errorf("no Claude credentials found in file or keychain/keyring")
}

// WriteTclaudeCredentials writes credentials to tclaude's own file.
func WriteTclaudeCredentials(data []byte) error {
	return writeTclaudeCredentials(data)
}

// DeleteClaudeCredentials removes credentials from the specified Claude store.
func DeleteClaudeCredentials(store string) error {
	switch credentialStore(store) {
	case storeFile:
		path := credentialsPath()
		if path == "" {
			return fmt.Errorf("credentials file path unavailable")
		}
		return os.Remove(path)

	case storeMacKeychain:
		username := currentUsername()
		args := []string{"delete-generic-password", "-s", "Claude Code-credentials"}
		if username != "" {
			args = append(args, "-a", username)
		}
		if out, err := exec.Command("security", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("keychain delete failed: %w (output: %s)", err, string(out))
		}
		return nil

	case storeLinuxKeyring:
		username := currentUsername()
		args := []string{"clear", "service", "Claude Code-credentials"}
		if username != "" {
			args = append(args, "account", username)
		}
		if out, err := exec.Command("secret-tool", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("keyring delete failed: %w (output: %s)", err, string(out))
		}
		return nil

	default:
		return fmt.Errorf("unknown credential store: %s", store)
	}
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

// writeCredentials writes credentials back to the store they came from.
func writeCredentials(store credentialStore, data []byte) error {
	switch store {
	case storeTclaude:
		slog.Info("writing refreshed credentials to tclaude file")
		return writeTclaudeCredentials(data)

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

// CacheTTL returns the active cache TTL based on credential mode.
func CacheTTL() time.Duration {
	if usingOwnCredentials() {
		return cacheTTLOwn
	}
	return cacheTTLShared
}

// loadCache returns cached usage if still fresh (within TTL).
func loadCache() *CachedUsage {
	return loadCacheWithTTL(CacheTTL())
}

// loadCacheStale returns cached usage regardless of age, or nil if missing/corrupt.
func loadCacheStale() *CachedUsage {
	return loadCacheWithTTL(0)
}

func loadCacheWithTTL(ttl time.Duration) *CachedUsage {
	path := cachePath()
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var cached CachedUsage
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}

	if ttl > 0 && time.Since(cached.LastAttemptAt) > ttl {
		return nil
	}

	return &cached
}

// saveCache persists usage data to disk using atomic write (tmp + rename).
func saveCache(usage *CachedUsage) {
	path := cachePath()
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	data, err := json.Marshal(usage)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}
	_ = os.Rename(tmpPath, path)
}

// FetchRawWithRetry calls the usage API. On 429 it attempts a token refresh
// if tclaude has its own credential file (safe, won't conflict with Claude Code)
// or when TCLAUDE_DEBUG_REFRESH=1 is set as a manual override.
func FetchRawWithRetry(token string) ([]byte, error) {
	body, err := FetchRaw(token)
	if err == nil {
		return body, nil
	}

	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		return nil, err
	}

	if !canRefreshToken() {
		return nil, err
	}

	slog.Info("FetchRawWithRetry: got 429, attempting token refresh")
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
// Creates a minimal cache entry if none exists.
func stampLastAttempt() {
	cached := loadCacheStale()
	if cached == nil {
		cached = &CachedUsage{}
	}
	cached.LastAttemptAt = time.Now()
	saveCache(cached)
}

// canRefreshToken returns true if token refresh is safe to perform.
// It's safe when tclaude has its own credential file (won't conflict with
// Claude Code), or when manually enabled via TCLAUDE_DEBUG_REFRESH=1.
func canRefreshToken() bool {
	return usingOwnCredentials() || os.Getenv("TCLAUDE_DEBUG_REFRESH") == "1"
}

// fetchWithRateLimitRetry fetches usage data. On 429 it attempts a token
// refresh if tclaude has its own credential file (safe) or TCLAUDE_DEBUG_REFRESH=1.
func fetchWithRateLimitRetry(token string) (*Response, error) {
	resp, err := fetchFunc(token)
	if err == nil {
		return resp, nil
	}

	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		return nil, err
	}

	if !canRefreshToken() {
		return nil, err
	}

	slog.Info("got 429, attempting token refresh")
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
// likely looking at the status bar. Skips the fetch if the disk cache is fresh.
func RefreshCache() {
	if cached := loadCache(); cached != nil {
		return // still fresh, nothing to do
	}
	token, err := getTokenFunc()
	if err != nil {
		slog.Warn("RefreshCache: failed to get access token", "error", err)
		stampLastAttempt()
		return
	}
	resp, err := fetchWithRateLimitRetry(token)
	if err != nil {
		slog.Warn("RefreshCache: failed to fetch usage data", "error", err)
		stampLastAttempt()
		return
	}
	saveCache(buildCachedUsage(resp))
}

// GetCached returns usage percentages, using a file cache (5 min TTL) to
// avoid hammering the API. On fetch errors, returns stale cached data if available.
func GetCached() (*CachedUsage, error) {
	if cached := loadCache(); cached != nil {
		return cached, nil
	}

	token, err := getTokenFunc()
	if err != nil {
		stampLastAttempt()
		if stale := loadCacheStale(); stale != nil && !stale.FetchedAt.IsZero() {
			return stale, fmt.Errorf("using stale cache: %w", err)
		}
		slog.Warn("no usage data available", "error", err)
		return nil, err
	}

	resp, err := fetchWithRateLimitRetry(token)
	if err != nil {
		stampLastAttempt()
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
