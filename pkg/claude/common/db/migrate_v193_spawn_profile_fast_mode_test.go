package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV192toV193AddsNullableFastMode(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN fast_mode`)
	mustExec(t, d, `UPDATE schema_version SET version = 192`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at)
		VALUES ('legacy-profile', 1785024000000000000, 1785024000000000000)`)

	require.NoError(t, migrateV192toV193(d))
	var mode sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT fast_mode FROM spawn_profiles WHERE name = 'legacy-profile'`).Scan(&mode))
	assert.False(t, mode.Valid, "legacy profiles inherit Codex config rather than pinning a tier")
	assert.Equal(t, 193, schemaVersion(d))
	require.NoError(t, migrateV192toV193(d), "partially applied migration converges")
}
