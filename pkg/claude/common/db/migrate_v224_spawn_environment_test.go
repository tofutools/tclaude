package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateV223ToV224AddsSpawnEnvironmentColumns(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v223.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (223)`)
	mustExec(t, d, `CREATE TABLE agent_groups (id INTEGER PRIMARY KEY) STRICT`)
	mustExec(t, d, `CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY) STRICT`)
	require.NoError(t, migrateV223toV224(d))
	for _, table := range []string{"agent_groups", "spawn_profiles"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'environment_json'`, table).Scan(&count))
		require.Equal(t, 1, count, table)
	}
	require.Equal(t, 224, schemaVersion(d))
}
