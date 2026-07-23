package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV150toV151AddsToolGovernance(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN tools`)
	mustExec(t, d, `ALTER TABLE roles DROP COLUMN tools`)
	mustExec(t, d, `UPDATE schema_version SET version = 150`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at) VALUES ('legacy', 'now', 'now')`)
	mustExec(t, d, `INSERT INTO roles (name, created_at, updated_at) VALUES ('legacy', 'now', 'now')`)

	require.NoError(t, migrateV150toV151(d))
	require.NoError(t, migrateV150toV151(d), "migration converges")
	assert.Equal(t, 151, schemaVersion(d))
	for _, table := range []string{"spawn_profiles", "roles"} {
		var value string
		require.NoError(t, d.QueryRow(`SELECT tools FROM `+table+` WHERE name = 'legacy'`).Scan(&value))
		assert.Empty(t, value)
	}
}
