package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV204toV205AddsCodexNativePermissionProfiles(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v204.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (204)`)

	require.NoError(t, migrateV204toV205(d))
	assert.Equal(t, 205, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'codex_native_permission_profiles'`).Scan(&count))
	assert.Equal(t, 1, count)
	var createdType string
	require.NoError(t, d.QueryRow(`SELECT type FROM pragma_table_info('codex_native_permission_profiles')
		WHERE name = 'created_at'`).Scan(&createdType))
	assert.Equal(t, "INTEGER", createdType)
	var cleanupType string
	require.NoError(t, d.QueryRow(`SELECT type FROM pragma_table_info('codex_native_permission_profiles')
		WHERE name = 'cleanup_pending'`).Scan(&cleanupType))
	assert.Equal(t, "INTEGER", cleanupType)
	require.NoError(t, migrateV204toV205(d), "partially applied migration converges")
}
