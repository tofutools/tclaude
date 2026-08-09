package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV200toV201AddsCodexAppServerRuntime(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v200.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (200)`)
	mustExec(t, d, `CREATE TABLE spawn_profiles (name TEXT PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV200toV201(d))
	assert.Equal(t, 201, schemaVersion(d))
	var columns int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles')
		WHERE name = 'codex_app_server'`).Scan(&columns))
	assert.Equal(t, 1, columns)
	var strict int
	require.NoError(t, d.QueryRow(`SELECT strict FROM pragma_table_list
		WHERE name = 'codex_app_server_runtimes'`).Scan(&strict))
	assert.Equal(t, 1, strict)
	require.NoError(t, migrateV200toV201(d), "partially applied migration converges")
}
