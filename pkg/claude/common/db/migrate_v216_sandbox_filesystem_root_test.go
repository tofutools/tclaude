package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV215toV216AddsAutomaticFilesystemRoot(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v216?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (215)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name) VALUES ('legacy')`)

	require.NoError(t, migrateV215toV216(d))
	assert.Equal(t, 216, schemaVersion(d))
	require.NoError(t, migrateV215toV216(d), "migration converges after partial application")
	var posture string
	require.NoError(t, d.QueryRow(`SELECT filesystem_root FROM sandbox_profiles WHERE name = 'legacy'`).Scan(&posture))
	assert.Empty(t, posture, "legacy profiles preserve automatic root derivation")
}
