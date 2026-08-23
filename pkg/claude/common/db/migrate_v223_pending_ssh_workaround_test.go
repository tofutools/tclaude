package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV223PendingSSHWorkaround(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v222.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (222)`)
	mustExec(t, d, `CREATE TABLE pending_spawns (label TEXT PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV222toV223(d))
	assert.Equal(t, 223, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = 'ssh_workaround'`,
	).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, migrateV222toV223(d), "partially applied migration converges")
}

func TestPendingSpawnSSHWorkaroundIntentRoundTrips(t *testing.T) {
	setupTestDB(t)
	for _, intent := range []bool{false, true} {
		intent := intent
		label := "pending-ssh-off"
		if intent {
			label = "pending-ssh-on"
		}
		require.NoError(t, InsertPendingSpawn(&PendingSpawn{
			Label: label, GroupID: 1, SSHWorkaround: &intent,
		}))
		got, err := GetPendingSpawn(label)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.SSHWorkaround)
		assert.Equal(t, intent, *got.SSHWorkaround)
	}
}
