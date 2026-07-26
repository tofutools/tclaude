package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV157toV158AddsCodexSSHWorkaround(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN ssh_workaround`)
	mustExec(t, d, `UPDATE schema_version SET version = 157`)

	require.NoError(t, migrateV157toV158(d))

	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles')
		WHERE name = 'ssh_workaround' AND type = 'INTEGER'`).Scan(&count))
	assert.Equal(t, 1, count)
	assert.Equal(t, 158, schemaVersion(d))
	require.NoError(t, migrateV157toV158(d), "partially applied migration converges")
}

func TestMigrateV158IsTheCurrentHead(t *testing.T) {
	require.Equal(t, 158, currentVersion, "tripwire: bump this with the next migration")
}
