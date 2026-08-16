package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV209AddsSpawnProfileRoleRef(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v208.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (208);
		CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO spawn_profiles (name) VALUES ('existing');
	`)

	require.NoError(t, migrateV208toV209(d))
	assert.Equal(t, 209, schemaVersion(d))
	assert.GreaterOrEqual(t, currentVersion, 209)
	assertRowValue(t, d, `SELECT role_ref FROM spawn_profiles WHERE name = 'existing'`, "")
	require.NoError(t, migrateV208toV209(d), "partially applied migration converges")
}
