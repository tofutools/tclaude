package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV137toV138AddsSubscriptionUsageExclusion(t *testing.T) {
	require.Equal(t, 138, currentVersion, "tripwire: bump this with the next migration")
	d, err := sql.Open("sqlite", "file:migrate-v138?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (137)`)
	mustExec(t, d, `CREATE TABLE subscription_usage_windows (
		sample_id INTEGER NOT NULL, window_name TEXT NOT NULL,
		used_percent REAL NOT NULL, observed_at TEXT NOT NULL,
		PRIMARY KEY(sample_id, window_name))`)
	mustExec(t, d, `INSERT INTO subscription_usage_windows
		(sample_id, window_name, used_percent, observed_at)
		VALUES (1, 'five_hour', 42, '2026-07-19T10:00:00Z')`)

	require.NoError(t, migrateV137toV138(d))
	assert.Equal(t, 138, schemaVersion(d))
	require.NoError(t, migrateV137toV138(d), "migration converges after a partial application")
	var excluded int
	require.NoError(t, d.QueryRow(`SELECT excluded FROM subscription_usage_windows`).Scan(&excluded))
	assert.Zero(t, excluded, "existing observations remain included")
}
