package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV205toV206AddsSingleDefaultSpawnGroup(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v205.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (205)`)
	mustExec(t, d, `CREATE TABLE agent_groups (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`)

	require.NoError(t, migrateV205toV206(d))
	assert.Equal(t, 206, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_spawn_group'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_agent_groups_one_default_spawn'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'codex_native_permission_profiles'`).Scan(&count))
	assert.Equal(t, 1, count, "upstream v205 table is retained/ensured")
	require.NoError(t, migrateV205toV206(d), "partially applied migration converges")
}

func TestMigrateV205toV206HealsPreRebaseFeatureV205(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "feature-v205.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (205)`)
	mustExec(t, d, `CREATE TABLE agent_groups (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		default_spawn_group INTEGER NOT NULL DEFAULT 0
	) STRICT`)
	mustExec(t, d, `CREATE UNIQUE INDEX idx_agent_groups_one_default_spawn
		ON agent_groups(default_spawn_group) WHERE default_spawn_group = 1`)

	require.NoError(t, migrateV205toV206(d))
	assert.Equal(t, 206, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'codex_native_permission_profiles'`).Scan(&count))
	assert.Equal(t, 1, count, "v206 heals the upstream DDL skipped by the collided feature v205")
}
