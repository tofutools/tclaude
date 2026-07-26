package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV158toV159AddsCodexSSHWorkaround(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN ssh_workaround`)
	mustExec(t, d, `UPDATE schema_version SET version = 158`)

	require.NoError(t, migrateV158toV159(d))

	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles')
		WHERE name = 'ssh_workaround' AND type = 'INTEGER'`).Scan(&count))
	assert.Equal(t, 1, count)
	assert.Equal(t, 159, schemaVersion(d))
	require.NoError(t, migrateV158toV159(d), "partially applied migration converges")
}
