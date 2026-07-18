package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV135toV136AddsPendingSpawnTaskRefColumns(t *testing.T) {
	require.Equal(t, 136, currentVersion, "tripwire: bump this with the next migration")
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	for _, col := range []string{"task_url", "task_label"} {
		var have int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = ?`, col,
		).Scan(&have))
		assert.Equal(t, 1, have, col)
	}

	// A rerun after an interrupted version bump converges without duplicate
	// column errors.
	mustExec(t, d, `UPDATE schema_version SET version = 135`)
	require.NoError(t, migrateV135toV136(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 136, version)
}

func TestPendingSpawnTaskRefRoundTrips(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, InsertPendingSpawn(&PendingSpawn{
		Label:     "pending-task",
		GroupID:   1,
		TaskURL:   "https://linear.app/acme/issue/TCL-568/spawn-task-race",
		TaskLabel: "TCL-568",
	}))
	got, err := GetPendingSpawn("pending-task")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "https://linear.app/acme/issue/TCL-568/spawn-task-race", got.TaskURL)
	assert.Equal(t, "TCL-568", got.TaskLabel)

	// Legacy shape: a row with no link stays the zero value through List.
	require.NoError(t, InsertPendingSpawn(&PendingSpawn{Label: "pending-plain", GroupID: 1}))
	all, err := ListPendingSpawns()
	require.NoError(t, err)
	byLabel := map[string]*PendingSpawn{}
	for _, p := range all {
		byLabel[p.Label] = p
	}
	require.Contains(t, byLabel, "pending-plain")
	assert.Empty(t, byLabel["pending-plain"].TaskURL)
	assert.Empty(t, byLabel["pending-plain"].TaskLabel)
}
