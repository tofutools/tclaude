package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV133toV134AddsSubscriptionUsageHistory(t *testing.T) {
	require.Equal(t, 134, currentVersion, "tripwire: bump this with the next migration")
	d, err := sql.Open("sqlite", "file:migrate-v134?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (133)`)

	require.NoError(t, migrateV133toV134(d))
	assert.Equal(t, 134, schemaVersion(d))
	require.NoError(t, migrateV133toV134(d), "migration converges after a partially applied schema change")

	for _, table := range []string{"subscription_usage_samples", "subscription_usage_windows"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
	var indexes int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_subscription_usage_samples_sampled_at'`).Scan(&indexes))
	assert.Equal(t, 1, indexes)

	result, err := d.Exec(`INSERT INTO subscription_usage_samples(provider, sampled_at)
		VALUES ('anthropic', '2026-07-17T12:00:00Z')`)
	require.NoError(t, err)
	id, err := result.LastInsertId()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO subscription_usage_windows(sample_id, window_name, used_percent, observed_at)
		VALUES (?, 'five_hour', 12, '2026-07-17T12:03:00Z')`, id)
	mustExec(t, d, `DELETE FROM subscription_usage_samples WHERE id = ?`, id)
	var children int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM subscription_usage_windows WHERE sample_id = ?`, id).Scan(&children))
	assert.Zero(t, children, "deleting a sample cascades to its windows")
}

func TestFreshSchemaHasSubscriptionUsageHistory(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	for _, table := range []string{"subscription_usage_samples", "subscription_usage_windows"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
}
