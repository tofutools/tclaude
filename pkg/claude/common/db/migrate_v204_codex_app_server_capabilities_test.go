package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV203toV204AddsCodexAppServerCapabilities(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v203.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (203)`)
	mustExec(t, d, `CREATE TABLE codex_app_server_runtimes
		(generation TEXT PRIMARY KEY, state TEXT NOT NULL) STRICT`)

	require.NoError(t, migrateV203toV204(d))
	assert.Equal(t, 204, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'codex_app_server_capabilities'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name = 'codex_app_server_capability_terminal_cleanup'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, migrateV203toV204(d), "partially applied migration converges")
}
