package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV165toV166AddsOptionalSandboxProfileAccessAxes(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v166?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (165)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]',
		environment_json TEXT NOT NULL DEFAULT '[]',
		agent_directories_json TEXT NOT NULL DEFAULT '[]',
		network_access TEXT NOT NULL DEFAULT '',
		includes_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles
		(name, network_access, created_at, updated_at)
		VALUES ('internet', 'internet', 'now', 'now'), ('offline', 'none', 'now', 'now')`)

	require.NoError(t, migrateV165toV166(d))
	rows, err := d.Query(`SELECT name, network_access, network_json, unix_sockets_json
		FROM sandbox_profiles ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	var got [][4]string
	for rows.Next() {
		var row [4]string
		require.NoError(t, rows.Scan(&row[0], &row[1], &row[2], &row[3]))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, [][4]string{
		{"internet", "internet", "", ""},
		{"offline", "none", "", ""},
	}, got, "the migration must not materialize or reinterpret either new axis")
	assert.Equal(t, 166, schemaVersion(d))
	require.NoError(t, migrateV165toV166(d), "partially applied migration converges")
}
