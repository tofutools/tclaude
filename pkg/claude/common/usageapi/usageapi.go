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

// fetchFunc, refreshTokenFunc, and credentialCandidatesFunc are swappable for testing.
var (
	fetchFunc        = Fetch
	refreshTokenFunc = RefreshAccessToken

	// credentialCandidatesFunc enumerates login-token sources in priority
	// order (file, then OS keychain/keyring) so the fetch path can fall
	// through to the next source when one's token is rejected by the API.
	credentialCandidatesFunc = credentialCandidates
)

// RateLimitError is returned when the API responds with HTTP 429.
type RateLimitError struct {
	Body string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (429): %s", e.Body)
}

// AuthError is returned when the API rejects the token (HTTP 401/403). It is
// the signal the fetch path uses to fall through to the next credential
// source — a stale or revoked token in one store should not block a valid
// token in another.
type AuthError struct {
	Status int
	Body   string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("token rejected (%d): %s", e.Status, e.Body)
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

// hasClaudeOAuth reports whether a credentials blob actually carries a Claude
// login token (claudeAiOauth.accessToken). Claude Code also writes an
// mcpOAuth-only ~/.claude/.credentials.json holding *MCP server* OAuth tokens
// — which is not the login credential and must not be mistaken for one. On
// macOS the login credential lives in the keychain, so when this file exists
// with only mcpOAuth, we must fall through to the OS secret store rather than
// treating the file as authoritative (otherwise GetAccessToken reports
// "no access token found in credentials"). See the usage-command regression.
func hasClaudeOAuth(data []byte) bool {
	return parseAccessToken(data) != ""
}

// parseAccessToken extracts claudeAiOauth.accessToken from a credentials blob,
// or "" if the blob is unparseable or carries no login token (e.g. an
// mcpOAuth-only file).
func parseAccessToken(data []byte) string {
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds.ClaudeAiOauth.AccessToken
}

// tokenCandidate is one credential source's login token plus where it came
// from (used for logging and, on refresh, where to write back).
type tokenCandidate struct {
	token string
	store credentialStore
}

// credentialCandidates enumerates every credential source that carries a
// usable login token, in priority order (file first, then the OS secret
// store), de-duplicated by token. A source with no claudeAiOauth token (e.g.
// the mcpOAuth-only ~/.claude/.credentials.json) is skipped — there's nothing
// to try. Returning all sources lets the fetch path fall through to the next
// one when the API rejects a token as unauthorized: a stale token left behind
// in the file (after Claude Code moved the login to the keychain) no longer
// traps us, because the keychain token is tried next.
func credentialCandidates() []tokenCandidate {
	var out []tokenCandidate
	seen := map[string]bool{}
	add := func(data []byte, store credentialStore) {
		tok := parseAccessToken(data)
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tokenCandidate{token: tok, store: store})
	}

	if path := credentialsPath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			add(data, storeFile)
		}
	}
	if data, store := osCredentialReader(); data != nil {
		add(data, store)
	}
	return out
}

// errNoCredentials is returned when no source yielded a login token.
func errNoCredentials() error {
	return fmt.Errorf("no Claude login token found in credentials file or OS keychain/keyring")
}

// osCredentialReader reads the Claude login credentials from the OS secret
// store for the current platform (macOS keychain / Linux keyring) and reports
// which store they came from. Returns nil data when unavailable or on an
// unsupported platform. Swappable for testing.
var osCredentialReader = readOSCredentials

func readOSCredentials() ([]byte, credentialStore) {
	switch runtime.GOOS {
	case "darwin":
		if data := readMacKeychain(); data != nil {
			return data, storeMacKeychain
		}
	case "linux":
		if data := readLinuxKeyring(); data != nil {
			return data, storeLinuxKeyring
		}
	}
	return nil, ""
}

// readCredentialsJSON returns the raw credentials JSON and where it was found,
// trying the file first, then falling back to the OS keychain/keyring. The
// file is only accepted when it actually holds a login token — an
// mcpOAuth-only file (see hasClaudeOAuth) is skipped so the keychain/keyring
// fallback still runs.
func readCredentialsJSON() (*credentialResult, error) {
	// Try the credentials file first — but only trust it if it carries a
	// login token; otherwise fall through to the OS secret store.
	path := credentialsPath()
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if hasClaudeOAuth(data) {
				slog.Debug("credentials read from file", "path", path)
				return &credentialResult{data: data, store: storeFile}, nil
			}
			slog.Debug("credentials file has no claudeAiOauth token; falling back to OS secret store", "path", path)
		}
	}

	// Fall back to the OS secret store (macOS keychain / Linux keyring).
	if data, store := osCredentialReader(); data != nil {
		slog.Debug("credentials read from OS secret store", "store", string(store))
		return &credentialResult{data: data, store: store}, nil
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

// GetAccessToken returns the first available login token (file, then OS
// keychain/keyring). Callers that fetch usage should prefer FetchUsage /
// FetchUsageRaw, which try every source and fall through on an unauthorized
// token; GetAccessToken only exposes the highest-priority token.
func GetAccessToken() (string, error) {
	cands := credentialCandidatesFunc()
	if len(cands) == 0 {
		return "", errNoCredentials()
	}
	return cands[0].token, nil
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
	defer func() { _ = resp.Body.Close() }()

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

// FetchUsageRaw fetches the raw usage JSON, trying every credential source
// (file, then keychain/keyring) and falling through to the next on an
// unauthorized token, plus the opt-in 429→refresh retry. This is the
// source-robust entry point for callers that don't already hold a token
// (e.g. `tclaude usage --json`).
func FetchUsageRaw() ([]byte, error) {
	body, err := fetchRawWithSources()
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

	slog.Info("FetchUsageRaw: got 429, attempting token refresh (TCLAUDE_DEBUG_REFRESH=1)")
	newToken, refreshErr := refreshTokenFunc()
	if refreshErr != nil {
		slog.Warn("FetchUsageRaw: token refresh failed", "error", refreshErr)
		return nil, err
	}

	slog.Info("FetchUsageRaw: retrying with new token")
	body, retryErr := FetchRaw(newToken)
	if retryErr != nil {
		slog.Warn("FetchUsageRaw: retry failed after token refresh", "error", retryErr)
		return nil, retryErr
	}

	slog.Info("FetchUsageRaw: succeeded after token refresh")
	return body, nil
}

// fetchRawWithSources tries each credential source's token against the usage
// API, advancing to the next source only when the API rejects the current
// token as unauthorized (an AuthError — stale/revoked). Rate-limit (429),
// network, and other errors are returned as-is: switching source won't fix
// them, and a 429 must not trigger extra API calls.
func fetchRawWithSources() ([]byte, error) {
	cands := credentialCandidatesFunc()
	if len(cands) == 0 {
		return nil, errNoCredentials()
	}
	var lastErr error
	for i, c := range cands {
		body, err := FetchRaw(c.token)
		if err == nil {
			return body, nil
		}
		lastErr = err
		var authErr *AuthError
		if errors.As(err, &authErr) && i < len(cands)-1 {
			slog.Warn("usage token rejected; trying next credential source",
				"rejected_store", string(c.store), "next_store", string(cands[i+1].store))
			continue
		}
		return nil, err
	}
	return nil, lastErr
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
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("usage API response read failed", "error", err)
		return nil, err
	}

	if resp.StatusCode == 429 {
		slog.Warn("usage API rate limited (429)", "body", string(body))
		return nil, &RateLimitError{Body: string(body)}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		slog.Warn("usage API rejected token (auth)", "status", resp.StatusCode, "body", string(body))
		return nil, &AuthError{Status: resp.StatusCode, Body: string(body)}
	}

	if resp.StatusCode != 200 {
		slog.Warn("usage API returned non-200", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	slog.Debug("usage API fetch succeeded", "status", resp.StatusCode)
	return body, nil
}

// FetchUsage fetches parsed usage, trying every credential source (file, then
// keychain/keyring) and falling through to the next on an unauthorized token,
// plus the opt-in 429→refresh retry. Source-robust entry point for callers
// that don't already hold a token (e.g. `tclaude usage`).
func FetchUsage() (*Response, error) {
	return fetchWithRateLimitRetry()
}

// FetchWithRetry calls the usage API with a single, caller-supplied token and
// on 429 refreshes the token and retries once. It does NOT do source
// fallback (the caller already chose the token); use FetchUsage for that.
func FetchWithRetry(token string) (*Response, error) {
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
	newToken, refreshErr := refreshTokenFunc()
	if refreshErr != nil {
		return nil, err
	}
	return fetchFunc(newToken)
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
	stampLastAttemptAt(err, time.Now())
}

func stampLastAttemptAt(err error, now time.Time) {
	cached := loadCacheStale()
	if cached == nil {
		cached = &CachedUsage{}
	}
	cached.LastAttemptAt = now
	if err != nil {
		cached.LastError = err.Error()
	}
	saveCache(cached)
}

// fetchWithSources tries each credential source's token against the usage API,
// advancing to the next source only when the API rejects the current token as
// unauthorized (an AuthError — stale/revoked). Rate-limit (429), network, and
// other errors are returned as-is: switching source won't fix them, and a 429
// must not trigger extra API calls.
func fetchWithSources() (*Response, error) {
	cands := credentialCandidatesFunc()
	if len(cands) == 0 {
		return nil, errNoCredentials()
	}
	var lastErr error
	for i, c := range cands {
		resp, err := fetchFunc(c.token)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var authErr *AuthError
		if errors.As(err, &authErr) && i < len(cands)-1 {
			slog.Warn("usage token rejected; trying next credential source",
				"rejected_store", string(c.store), "next_store", string(cands[i+1].store))
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

// fetchWithRateLimitRetry fetches usage data across all credential sources
// (see fetchWithSources). On 429 it attempts a token refresh only when
// TCLAUDE_DEBUG_REFRESH=1 is set (disabled by default because refreshing from
// tclaude invalidates Claude Code's in-memory refresh token).
func fetchWithRateLimitRetry() (*Response, error) {
	resp, err := fetchWithSources()
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

// buildCachedUsage converts an API Response to a CachedUsage. Windows the
// response omits are carried forward from the last-known cache (see
// carryForwardWindows) so the dashboard's 5h/7d bars don't flicker out the
// instant a poll happens to drop a bucket.
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
	carryForwardWindows(cached, loadCacheStale(), now)
	return cached
}

// carryForwardWindows fills in any rolling-limit window that a fresh
// reading omitted with the last-known one from prev. The Anthropic usage
// API (and Claude Code's statusline) drops a window's bucket when it has
// nothing fresh to report, which made the dashboard's 7d (weekly) bar
// flicker out even while there was still usage inside the window. Rather
// than vanishing the bar the moment one poll omits it, we hold the
// last-known bucket — but only while it still describes real, current
// usage: see carryForwardWindow for the per-window rule. now is injected
// for tests.
func carryForwardWindows(fresh, prev *CachedUsage, now time.Time) {
	if prev == nil {
		return
	}
	fresh.FiveHour = carryForwardWindow(fresh.FiveHour, prev.FiveHour, now)
	fresh.SevenDay = carryForwardWindow(fresh.SevenDay, prev.SevenDay, now)
	fresh.SevenDaySonnet = carryForwardWindow(fresh.SevenDaySonnet, prev.SevenDaySonnet, now)
}

// carryForwardWindow decides which bucket to keep for a single window. A
// fresh reading always wins. When the fresh reading omits the window, the
// previous bucket is carried forward only while it still describes the
// current rolling period — i.e. its reset lies in the future. A missing or
// past-reset previous bucket is dropped (returns nil), so the bar
// disappears once the period it described has elapsed. The percent is
// deliberately NOT a condition: a 0% window with a future reset is real,
// current data (the account just hasn't spent into it yet), so it is
// carried forward and keeps its remaining-time hint rather than flickering
// to a hintless zero-fill on the next omitting render. This mirrors the
// dashboard read path's liveUsageWindow, which now likewise keys liveness
// on a future reset, not a nonzero percent. For the 7d window this is "keep
// it as long as its week hasn't reset"; the 5h window self-bounds at its
// own (much shorter) reset.
func carryForwardWindow(fresh, prev *CachedBucket, now time.Time) *CachedBucket {
	if fresh != nil {
		// A fresh reading wins — but the Anthropic weekly bucket sometimes
		// arrives with a real percent and NO (or an already-elapsed)
		// resets_at, which would overwrite a previously-cached reset that is
		// still in the future for the same window. Don't downgrade: keep the
		// fresh percent but graft the previous window's still-future reset,
		// so the bar keeps its remaining-time hint and still expires itself
		// when that period actually ends. When the fresh reading carries its
		// own future reset, or there's no better prior reset to borrow, the
		// fresh bucket is returned untouched.
		if !fresh.ResetsAt.After(now) && prev != nil && prev.ResetsAt.After(now) {
			return &CachedBucket{Pct: fresh.Pct, ResetsAt: prev.ResetsAt}
		}
		return fresh
	}
	if prev == nil {
		return nil
	}
	if !prev.ResetsAt.After(now) {
		return nil
	}
	return prev
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

	resp, err := fetchWithRateLimitRetry()
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
	existing := loadCacheStale()
	// Carry forward any window this statusline render omitted (e.g. the 7d
	// bucket the API drops when it has nothing fresh to report) so the
	// dashboard bars don't flicker out — see carryForwardWindows.
	carryForwardWindows(cached, existing, now)
	// Preserve extra usage data from any existing cache entry
	if existing != nil {
		cached.ExtraUsage = existing.ExtraUsage
	}
	saveCache(cached)
}

// Peek returns whatever usage data is cached in SQLite, regardless of
// age, or nil if nothing is cached or the cached blob is corrupt.
// Unlike GetCached it never makes a network call — it is the cheap
// read path for consumers (e.g. agentd's dashboard snapshot) that want
// the last-known figures without blocking on the usage API. Callers
// decide for themselves whether CachedUsage.FetchedAt is fresh enough.
func Peek() *CachedUsage {
	return loadCacheStale()
}

// GetCached returns usage percentages, using a file cache (5 min TTL) to
// avoid hammering the API. On fetch errors, returns stale cached data if available.
func GetCached() (*CachedUsage, error) {
	if cached := loadCache(); cached != nil {
		return cached, nil
	}

	resp, err := fetchWithRateLimitRetry()
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
