package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageCacheCRUD(t *testing.T) {
	setupTestDB(t)

	// Initially empty
	row, err := LoadUsageCache()
	require.NoError(t, err, "LoadUsageCache")
	assert.Nil(t, row, "expected nil for empty cache")

	// Save
	data := json.RawMessage(`{"five_hour":{"pct":42}}`)
	now := time.Now().Truncate(time.Millisecond)
	require.NoError(t, SaveUsageCache(data, now, now), "SaveUsageCache")

	// Load back
	row, err = LoadUsageCache()
	require.NoError(t, err, "LoadUsageCache")
	require.NotNil(t, row, "expected non-nil cache row")
	assert.Equal(t, `{"five_hour":{"pct":42}}`, string(row.Data), "data")

	// Update
	data2 := json.RawMessage(`{"five_hour":{"pct":77}}`)
	require.NoError(t, SaveUsageCache(data2, now, now), "SaveUsageCache update")
	row, _ = LoadUsageCache()
	assert.Equal(t, `{"five_hour":{"pct":77}}`, string(row.Data), "after update, data")

	// Delete
	require.NoError(t, DeleteUsageCache(), "DeleteUsageCache")
	row, _ = LoadUsageCache()
	assert.Nil(t, row, "expected nil after delete")
}

func TestLoadDashboardUsageCaches(t *testing.T) {
	setupTestDB(t)

	usage, codex, hasHistory, err := LoadDashboardUsageCaches()
	require.NoError(t, err)
	assert.Nil(t, usage)
	assert.Nil(t, codex)
	assert.False(t, hasHistory)

	fetchedAt := time.Date(2026, 7, 16, 10, 0, 0, 123_000_000, time.UTC)
	lastAttemptAt := fetchedAt.Add(time.Minute)
	require.NoError(t, SaveUsageCache(
		json.RawMessage(`{"five_hour":{"pct":42}}`), fetchedAt, lastAttemptAt,
	))

	usage, codex, hasHistory, err = LoadDashboardUsageCaches()
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.JSONEq(t, `{"five_hour":{"pct":42}}`, string(usage.Data))
	assert.Equal(t, fetchedAt, usage.FetchedAt)
	assert.Equal(t, lastAttemptAt, usage.LastAttemptAt)
	assert.Nil(t, codex, "the two cache rows remain independently optional")
	assert.False(t, hasHistory, "cache presence alone does not identify a subscription")

	observedAt := fetchedAt.Add(2 * time.Minute)
	stored, err := SaveCodexUsageCacheIfNewer(
		json.RawMessage(`{"primary":{"used_percent":17}}`), observedAt, "rollout.jsonl",
	)
	require.NoError(t, err)
	require.True(t, stored)

	usage, codex, hasHistory, err = LoadDashboardUsageCaches()
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, codex)
	assert.JSONEq(t, `{"primary":{"used_percent":17}}`, string(codex.Data))
	assert.Equal(t, observedAt, codex.ObservedAt)
	assert.False(t, codex.UpdatedAt.IsZero())
	assert.Equal(t, "rollout.jsonl", codex.Source)
	assert.False(t, hasHistory, "a Codex cache without quota windows remains pay-per-token shaped")

	require.NoError(t, DeleteUsageCache())
	usage, codex, hasHistory, err = LoadDashboardUsageCaches()
	require.NoError(t, err)
	assert.Nil(t, usage, "Codex remains readable without a Claude cache row")
	require.NotNil(t, codex)
	assert.Equal(t, observedAt, codex.ObservedAt)
	assert.False(t, hasHistory)

	stored, err = SaveCodexUsageCacheIfNewer(
		json.RawMessage(`{"primary":{"used_percent":19}}`), observedAt.Add(time.Minute), "rollout.jsonl",
		SubscriptionUsageWindow{Name: "five_hour", Duration: 5 * time.Hour, UsedPercent: 19},
	)
	require.NoError(t, err)
	require.True(t, stored)
	_, _, hasHistory, err = LoadDashboardUsageCaches()
	require.NoError(t, err)
	assert.True(t, hasHistory, "retained quota windows identify a subscription account")
}

func TestSaveCodexUsageCacheRecordsOpenAIHistory(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	stored, err := SaveCodexUsageCacheIfNewer(
		json.RawMessage(`{"Observed":"`+now.Format(time.RFC3339Nano)+`"}`),
		now,
		"rollout.jsonl",
		SubscriptionUsageWindow{
			Name: "seven_day", Duration: 7 * 24 * time.Hour,
			UsedPercent: 27, ResetsAt: now.Add(5 * 24 * time.Hour),
		},
	)
	require.NoError(t, err)
	assert.True(t, stored)

	d, err := Open()
	require.NoError(t, err)
	var provider, name string
	var pct float64
	require.NoError(t, d.QueryRow(`SELECT s.provider, w.window_name, w.used_percent
		FROM subscription_usage_samples s JOIN subscription_usage_windows w ON w.sample_id = s.id`).
		Scan(&provider, &name, &pct))
	assert.Equal(t, SubscriptionProviderOpenAI, provider)
	assert.Equal(t, "seven_day", name)
	assert.Equal(t, 27.0, pct)
}

func TestSaveCodexUsageCacheOlderSnapshotCanFillMissingHistoryWindow(t *testing.T) {
	setupTestDB(t)
	base := time.Now().UTC().Truncate(SubscriptionUsageSampleInterval)
	newer := base.Add(10 * time.Minute)
	stored, err := SaveCodexUsageCacheIfNewer(
		json.RawMessage(`{"observed":"newer"}`), newer, "newer.jsonl",
		SubscriptionUsageWindow{Name: "five_hour", UsedPercent: 12},
	)
	require.NoError(t, err)
	assert.True(t, stored)

	older := base.Add(9 * time.Minute)
	stored, err = SaveCodexUsageCacheIfNewer(
		json.RawMessage(`{"observed":"older"}`), older, "older.jsonl",
		SubscriptionUsageWindow{Name: "seven_day", UsedPercent: 34},
	)
	require.NoError(t, err)
	assert.False(t, stored, "older snapshot must not replace the current-value cache")

	row, err := LoadCodexUsageCache()
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, newer, row.ObservedAt)
	assert.Equal(t, "newer.jsonl", row.Source)

	d, err := Open()
	require.NoError(t, err)
	var windows int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_windows`).Scan(&windows))
	assert.Equal(t, 2, windows, "older weekly observation fills the child the newer five-hour snapshot omitted")
}

func TestTryClaimUsageFetch_FirstClaim(t *testing.T) {
	setupTestDB(t)

	ttl := 5 * time.Minute

	// First claim on empty table should succeed
	claimed, err := TryClaimUsageFetch(ttl)
	require.NoError(t, err, "TryClaimUsageFetch")
	assert.True(t, claimed, "expected first claim to succeed")

	// Second claim within TTL should fail
	claimed, err = TryClaimUsageFetch(ttl)
	require.NoError(t, err, "TryClaimUsageFetch")
	assert.False(t, claimed, "expected second claim within TTL to fail")
}

func TestTryClaimUsageFetch_ExpiredClaim(t *testing.T) {
	setupTestDB(t)

	ttl := 5 * time.Minute

	// Seed with an expired entry
	oldTime := time.Now().Add(-ttl - time.Minute)
	data := json.RawMessage(`{}`)
	require.NoError(t, SaveUsageCache(data, oldTime, oldTime), "SaveUsageCache")

	// Claim should succeed since entry is expired
	claimed, err := TryClaimUsageFetch(ttl)
	require.NoError(t, err, "TryClaimUsageFetch")
	assert.True(t, claimed, "expected claim on expired entry to succeed")
}

func TestTryClaimUsageFetch_FreshEntry(t *testing.T) {
	setupTestDB(t)

	ttl := 5 * time.Minute

	// Seed with a fresh entry
	now := time.Now()
	data := json.RawMessage(`{"five_hour":{"pct":50}}`)
	require.NoError(t, SaveUsageCache(data, now, now), "SaveUsageCache")

	// Claim should fail since entry is fresh
	claimed, err := TryClaimUsageFetch(ttl)
	require.NoError(t, err, "TryClaimUsageFetch")
	assert.False(t, claimed, "expected claim on fresh entry to fail")

	// Verify data wasn't corrupted
	row, err := LoadUsageCache()
	require.NoError(t, err, "LoadUsageCache")
	assert.Equal(t, `{"five_hour":{"pct":50}}`, string(row.Data), "data changed unexpectedly")
}

func TestGitCacheCRUD(t *testing.T) {
	setupTestDB(t)

	// Initially empty
	row, err := LoadGitCache("abc123")
	require.NoError(t, err, "LoadGitCache")
	assert.Nil(t, row, "expected nil for empty cache")

	// Save
	data := json.RawMessage(`{"branch":"main","repo_url":"https://github.com/test/repo"}`)
	now := time.Now().Truncate(time.Millisecond)
	require.NoError(t, SaveGitCache("abc123", data, now), "SaveGitCache")

	// Load back
	row, err = LoadGitCache("abc123")
	require.NoError(t, err, "LoadGitCache")
	require.NotNil(t, row, "expected non-nil cache row")
	assert.Equal(t, `{"branch":"main","repo_url":"https://github.com/test/repo"}`, string(row.Data), "data")

	// Different key returns nil
	row, _ = LoadGitCache("other-key")
	assert.Nil(t, row, "expected nil for different key")

	// Update same key
	data2 := json.RawMessage(`{"branch":"feature"}`)
	require.NoError(t, SaveGitCache("abc123", data2, now), "SaveGitCache update")
	row, _ = LoadGitCache("abc123")
	assert.Equal(t, `{"branch":"feature"}`, string(row.Data), "after update, data")
}

func TestGitCache_MultipleRepos(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	require.NoError(t, SaveGitCache("repo-a", json.RawMessage(`{"branch":"main"}`), now), "SaveGitCache repo-a")
	require.NoError(t, SaveGitCache("repo-b", json.RawMessage(`{"branch":"develop"}`), now), "SaveGitCache repo-b")

	rowA, _ := LoadGitCache("repo-a")
	rowB, _ := LoadGitCache("repo-b")

	assert.Equal(t, `{"branch":"main"}`, string(rowA.Data), "repo-a data")
	assert.Equal(t, `{"branch":"develop"}`, string(rowB.Data), "repo-b data")
}

func TestSchemaV1ToV2Migration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ResetForTest()

	// Open creates v2 schema from scratch
	d, err := Open()
	require.NoError(t, err, "Open")

	// Verify the cache tables exist by inserting into them
	_, err = d.Exec(`INSERT INTO usage_cache (id, data, fetched_at, last_attempt_at) VALUES (1, '{}', '', '')`)
	require.NoError(t, err, "insert into usage_cache")
	_, err = d.Exec(`INSERT INTO git_cache (repo_hash, data, fetched_at) VALUES ('test', '{}', '')`)
	require.NoError(t, err, "insert into git_cache")

	// Verify schema version is 2
	var ver int
	require.NoError(t, d.QueryRow("SELECT version FROM schema_version").Scan(&ver), "schema_version")
	require.Equal(t, currentVersion, ver, "expected version %d, got %d", currentVersion, ver)
}
