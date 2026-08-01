package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV178toV179AddsOptInResourceLimits(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v179?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (178)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name) VALUES ('legacy')`)

	require.NoError(t, migrateV178toV179(d))
	assert.Equal(t, 179, schemaVersion(d))
	require.NoError(t, migrateV178toV179(d), "migration converges after partial application")
	var payload string
	require.NoError(t, d.QueryRow(`SELECT resource_limits_json FROM sandbox_profiles WHERE name = 'legacy'`).Scan(&payload))
	assert.Equal(t, "{}", payload, "legacy profiles remain fully opted out")
}
