package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV176toV177AddsSpawnProfileOperatorOnly(t *testing.T) {
	d, err := sql.Open("sqlite",
		"file:migrate-v177?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (176);
		CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO spawn_profiles(id, name) VALUES (1, 'existing');
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV176toV177(d))
	assert.Equal(t, 177, schemaVersion(d))

	var operatorOnly int
	require.NoError(t, d.QueryRow(`SELECT operator_only FROM spawn_profiles WHERE id = 1`).Scan(&operatorOnly))
	assert.Zero(t, operatorOnly, "existing profiles must remain agent-spawnable")
	require.NoError(t, migrateV176toV177(d), "migration must be idempotent")
}
