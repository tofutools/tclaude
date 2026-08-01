package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV179toV180AddsOpenCodeResourceCgroup(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `ALTER TABLE opencode_runtimes DROP COLUMN resource_cgroup_dir`)
	mustExec(t, d, `UPDATE schema_version SET version = 179`)
	require.NoError(t, migrateV179toV180(d))
	var have int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('opencode_runtimes') WHERE name = 'resource_cgroup_dir'`).Scan(&have))
	assert.Equal(t, 1, have)
	assert.Equal(t, 180, schemaVersion(d))
}
