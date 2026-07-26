package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV159toV160AddsSpoolBindings(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE agent_spool_bindings`)
	mustExec(t, d, `UPDATE schema_version SET version = 159`)

	require.NoError(t, migrateV159toV160(d))

	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'agent_spool_bindings'`).Scan(&count))
	assert.Equal(t, 1, count)
	assert.Equal(t, 160, schemaVersion(d))
	require.NoError(t, migrateV159toV160(d), "partially applied migration converges")
}

func TestSpoolBindingLifecycle(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, CreateSpoolBinding("spool-a", "conv-1", "/tmp/spool-a"))
	require.NoError(t, CreateSpoolBinding("spool-b", "conv-2", "/tmp/spool-b"))
	require.Error(t, CreateSpoolBinding("spool-a", "conv-3", "/tmp/other"),
		"a spool id is a capability and must never be reusable")

	active, err := ListActiveSpoolBindings()
	require.NoError(t, err)
	require.Len(t, active, 2)

	n, err := RevokeSpoolBindingsForConv("conv-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	active, err = ListActiveSpoolBindings()
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "spool-b", active[0].SpoolID)
	assert.Equal(t, "conv-2", active[0].ConvID)
	assert.Equal(t, "/tmp/spool-b", active[0].Dir)
	assert.False(t, active[0].CreatedAt.IsZero())
}

func TestMigrateV160IsTheCurrentHead(t *testing.T) {
	require.Equal(t, 160, currentVersion, "tripwire: bump this with the next migration")
}
