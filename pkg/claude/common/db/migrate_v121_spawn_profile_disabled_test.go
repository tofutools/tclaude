package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV120ToV121SpawnProfileDisabled(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v121?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (120);
		CREATE TABLE spawn_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);
		INSERT INTO spawn_profiles (name) VALUES ('paused');
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV120toV121(d))
	require.NoError(t, migrateV120toV121(d), "half-applied migration converges")

	var reason string
	require.NoError(t, d.QueryRow(`SELECT disabled_reason FROM spawn_profiles WHERE name = 'paused'`).Scan(&reason))
	assert.Empty(t, reason)
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 121, ver)
}
