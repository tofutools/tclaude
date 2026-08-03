package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV186toV187AddsOptInDarwinMachRegister(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v187?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (186)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name) VALUES ('legacy')`)

	require.NoError(t, migrateV186toV187(d))
	assert.Equal(t, 187, schemaVersion(d))
	require.NoError(t, migrateV186toV187(d), "migration converges after partial application")
	var allowed bool
	require.NoError(t, d.QueryRow(`SELECT darwin_allow_mach_register FROM sandbox_profiles WHERE name = 'legacy'`).Scan(&allowed))
	assert.False(t, allowed, "legacy profiles remain opted out")
}
