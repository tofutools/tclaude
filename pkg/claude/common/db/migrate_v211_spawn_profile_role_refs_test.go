package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV210BackfillsSpawnProfileRoleRefs(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v210.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version VALUES (210);`)
	mustExec(t, d, `CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL, role_ref TEXT NOT NULL DEFAULT '');`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, role_ref) VALUES ('review-kit', 'reviewer'), ('plain', '');`)

	require.NoError(t, migrateV210toV211(d))
	assert.Equal(t, 211, schemaVersion(d))
	var refs string
	require.NoError(t, d.QueryRow(`SELECT role_refs FROM spawn_profiles WHERE name = 'review-kit'`).Scan(&refs))
	assert.JSONEq(t, `["reviewer"]`, refs)
	require.NoError(t, d.QueryRow(`SELECT role_refs FROM spawn_profiles WHERE name = 'plain'`).Scan(&refs))
	assert.JSONEq(t, `[]`, refs)
}
