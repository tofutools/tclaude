package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV119ToV120SpawnProfileAliases(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v120?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (119);
		CREATE TABLE spawn_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);
		INSERT INTO spawn_profiles (name) VALUES ('primary');
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV119toV120(d))
	require.NoError(t, migrateV119toV120(d), "half-applied migration converges")

	var tableCount int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profile_aliases'`).Scan(&tableCount))
	assert.Equal(t, 1, tableCount)
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 120, ver)

	_, err = d.Exec(`INSERT INTO spawn_profile_aliases (alias, profile_id)
		VALUES ('reviewer', (SELECT id FROM spawn_profiles WHERE name = 'primary'))`)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO spawn_profiles (name) VALUES ('reviewer')`)
	require.Error(t, err, "an alias reserves the primary-name namespace")
	_, err = d.Exec(`INSERT INTO spawn_profile_aliases (alias, profile_id)
		VALUES ('primary', (SELECT id FROM spawn_profiles WHERE name = 'primary'))`)
	require.Error(t, err, "a primary name reserves the alias namespace")
}
