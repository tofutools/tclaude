package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV216toV217AddsPendingFastModeObservation(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v216.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (216)`)
	mustExec(t, d, `CREATE TABLE pending_spawns (label TEXT PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV216toV217(d))
	assert.Equal(t, 217, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = 'fast_mode_at_launch'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, migrateV216toV217(d), "partially applied migration converges")
}

func TestMigrateV216toV217ToleratesPartialSchemaWithoutPendingSpawns(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v216-partial.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (216)`)

	require.NoError(t, migrateV216toV217(d))
	assert.Equal(t, 217, schemaVersion(d))
}
