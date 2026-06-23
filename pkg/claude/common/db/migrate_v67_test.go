package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV66toV67_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts the export_jobs table landed. v67 is head, so this is where
// the literal currentVersion pin now lives — the tripwire the next migration's
// author moves forward into their own v68 test.
func TestMigrateV66toV67_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v66 test.
	require.Equal(t, 67, currentVersion, "currentVersion is 67")

	var haveTable int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'export_jobs'`,
	).Scan(&haveTable))
	assert.Equal(t, 1, haveTable, "fresh schema has export_jobs")

	// Spot-check a couple of columns exist.
	for _, col := range []string{"conv_id", "status", "artifact_path", "created_at"} {
		var haveCol int
		require.NoErrorf(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('export_jobs') WHERE name = ?`, col,
		).Scan(&haveCol), "probe export_jobs.%s", col)
		assert.Equalf(t, 1, haveCol, "export_jobs has %s", col)
	}
}
