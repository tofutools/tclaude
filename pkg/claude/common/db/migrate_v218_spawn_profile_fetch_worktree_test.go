package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV217toV218AddsSpawnProfileFetchLatestWorktree(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v217.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (217)`)
	mustExec(t, d, `CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV217toV218(d))
	assert.Equal(t, 218, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'fetch_latest_worktree'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, migrateV217toV218(d), "partially applied migration converges")
}
