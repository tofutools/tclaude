// Package usageapi provides access to the Anthropic OAuth usage API
// for fetching subscription usage limits.
package usageapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

const (
	usageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	cacheTTL      = 60 * time.Second
)

// fetchFunc and getTokenFunc are swappable for testing.
var (
	fetchFunc    = Fetch
	getTokenFunc = GetAccessToken
)

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

// readCredentialsJSON returns the raw credentials JSON, trying the file first
// and falling back to the macOS keychain if the file is unavailable.
func readCredentialsJSON() ([]byte, error) {
	// Try the credentials file first
	path := credentialsPath()
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}

	// On macOS, fall back to the keychain
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err == nil {
			return []byte(strings.TrimSpace(string(out))), nil
		}
	}

	return nil, fmt.Errorf("cannot read credentials from file or keychain")
}

// GetAccessToken reads the OAuth access token from Claude credentials
func GetAccessToken() (string, error) {
	data, err := readCredentialsJSON()
	if err != nil {
		return "", err
	}

	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("cannot parse credentials: %w", err)
	}

	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no access token found in credentials")
	}

	return creds.ClaudeAiOauth.AccessToken, nil
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

	if resp.StatusCode != 200 {
		slog.Warn("usage API returned non-200", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	slog.Debug("usage API fetch succeeded", "status", resp.StatusCode)
	return body, nil
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

// RefreshCache updates the cache if stale. Called from hooks when the user is
// likely looking at the status bar. Skips the fetch if the disk cache is fresh.
func RefreshCache() {
	if cached := loadCache(); cached != nil {
		return // still fresh, nothing to do
	}
	token, err := getTokenFunc()
	if err != nil {
		stampLastAttempt()
		return
	}
	resp, err := fetchFunc(token)
	if err != nil {
		stampLastAttempt()
		return
	}
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
	saveCache(cached)
}

// GetCached returns usage percentages, using a file cache to avoid hammering the API.
// On fetch errors (e.g. 429 rate limit), returns stale cached data if available.
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

	resp, err := fetchFunc(token)
	if err != nil {
		stampLastAttempt()
		if stale := loadCacheStale(); stale != nil && !stale.FetchedAt.IsZero() {
			return stale, fmt.Errorf("using stale cache: %w", err)
		}
		slog.Warn("no usage data available", "error", err)
		return nil, err
	}

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

	saveCache(cached)
	return cached, nil
}
