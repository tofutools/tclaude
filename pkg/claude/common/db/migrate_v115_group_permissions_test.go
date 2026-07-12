package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateV114toV115AddsGroupPermissions(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE agent_group_permissions`)
	mustExec(t, d, `UPDATE schema_version SET version = 114`)

	require.NoError(t, migrateV114toV115(d))
	require.NoError(t, migrateV114toV115(d), "half-applied migration converges")

	var have int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_group_permissions'`).Scan(&have))
	require.Equal(t, 1, have)
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 115, version)
}
