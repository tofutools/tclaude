package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV123toV124AddsSandboxProfileNetworkAccess(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v124?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (123)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]',
		environment_json TEXT NOT NULL DEFAULT '[]',
		agent_directories_json TEXT NOT NULL DEFAULT '[]',
		includes_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name, created_at, updated_at) VALUES ('existing', 'now', 'now')`)

	require.NoError(t, migrateV123toV124(d))
	var network string
	require.NoError(t, d.QueryRow(`SELECT network_access FROM sandbox_profiles WHERE name = 'existing'`).Scan(&network))
	assert.Empty(t, network)
	assert.Equal(t, 124, schemaVersion(d))
	require.NoError(t, migrateV123toV124(d), "migration is idempotent after a partially applied schema change")
}
