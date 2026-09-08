package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateV227ToV228AddsSubscriptionUsageUnits(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v227.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (227)`)
	mustExec(t, d, `CREATE TABLE subscription_usage_windows (
		sample_id INTEGER NOT NULL, window_name TEXT NOT NULL,
		used_percent REAL NOT NULL, PRIMARY KEY(sample_id, window_name)) STRICT`)
	require.NoError(t, migrateV227toV228(d))
	require.NoError(t, migrateV227toV228(d), "partially applied migration converges")
	for _, column := range []string{"used_units", "limit_units"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('subscription_usage_windows') WHERE name = ?`, column).Scan(&count))
		require.Equal(t, 1, count, column)
	}
	require.Equal(t, 228, schemaVersion(d))
}
