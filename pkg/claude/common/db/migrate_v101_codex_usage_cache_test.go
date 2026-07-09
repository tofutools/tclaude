package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV100toV101_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it reaches at least v101. The literal currentVersion
// tripwire moved forward to the v102 head test
// (migrate_v102_template_agent_profile_inline_test.go).
func TestMigrateV100toV101_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.GreaterOrEqual(t, ver, 101, "fresh DB migrates through v101")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'codex_usage_cache'`).Scan(&have))
	assert.Equal(t, 1, have, "fresh schema has codex_usage_cache")
}

func TestMigrateV100toV101_AddsCodexUsageCache(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	mustExec(t, d, `DROP TABLE codex_usage_cache`)
	mustExec(t, d, `UPDATE schema_version SET version = 100`)

	require.NoError(t, migrateV100toV101(d), "v100->v101")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'codex_usage_cache'`).Scan(&have))
	assert.Equal(t, 1, have, "codex_usage_cache table added")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 101, ver, "version advanced")

	require.NoError(t, migrateV100toV101(d), "v100->v101 re-run is a clean no-op")
}

func TestCodexUsageCache_OnlyNewerObservedWins(t *testing.T) {
	setupTestDB(t)
	older := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)

	ok, err := SaveCodexUsageCacheIfNewer(json.RawMessage(`{"observed":"older"}`), older, "old.jsonl")
	require.NoError(t, err)
	assert.True(t, ok, "first write stores")

	ok, err = SaveCodexUsageCacheIfNewer(json.RawMessage(`{"observed":"stale"}`), older.Add(-time.Minute), "stale.jsonl")
	require.NoError(t, err)
	assert.False(t, ok, "stale write ignored")

	ok, err = SaveCodexUsageCacheIfNewer(json.RawMessage(`{"observed":"newer"}`), newer, "new.jsonl")
	require.NoError(t, err)
	assert.True(t, ok, "newer write stores")

	row, err := LoadCodexUsageCache()
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.JSONEq(t, `{"observed":"newer"}`, string(row.Data))
	assert.Equal(t, newer, row.ObservedAt)
	assert.Equal(t, "new.jsonl", row.Source)
}
