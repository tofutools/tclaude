package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV113toV114AddsSandboxProfileAgentDirectories(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v114?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (113)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]', environment_json TEXT NOT NULL DEFAULT '[]',
		includes_json TEXT NOT NULL DEFAULT '[]', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)

	require.NoError(t, migrateV113toV114(d))
	var have int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'agent_directories_json'`).Scan(&have))
	require.Equal(t, 1, have)
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 114, version)
}
