package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV181toV182AddsSpawnProfileAndPendingContext(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN startup_context`)
	mustExec(t, d, `ALTER TABLE pending_spawns DROP COLUMN profile_context`)
	mustExec(t, d, `UPDATE schema_version SET version = 181`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at)
		VALUES ('legacy-profile', 1785024000000000000, 1785024000000000000)`)
	mustExec(t, d, `INSERT INTO pending_spawns (label, group_id, created_at)
		VALUES ('legacy-pending', 1, 1785024000000000000)`)

	require.NoError(t, migrateV181toV182(d))
	var profileContext, pendingContext string
	require.NoError(t, d.QueryRow(`SELECT startup_context FROM spawn_profiles WHERE name = 'legacy-profile'`).Scan(&profileContext))
	require.NoError(t, d.QueryRow(`SELECT profile_context FROM pending_spawns WHERE label = 'legacy-pending'`).Scan(&pendingContext))
	assert.Empty(t, profileContext)
	assert.Empty(t, pendingContext)
	assert.Equal(t, 182, schemaVersion(d))
	require.NoError(t, migrateV181toV182(d), "partially applied migration converges")
}
