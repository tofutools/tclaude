package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV152toV153OpenCodeStepRemovals(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_usage_step_removals'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_opencode_usage_step_removals_removed'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
}

func TestMigrateV152toV153_UpgradesRealV152Database(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `DROP INDEX idx_opencode_usage_step_removals_removed`)
	mustExec(t, d, `DROP TABLE opencode_usage_step_removals`)
	mustExec(t, d, `UPDATE schema_version SET version = 152`)

	require.NoError(t, migrateV152toV153(d), "v152->v153")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_usage_step_removals'`,
	).Scan(&have))
	assert.Equal(t, 1, have, "upgrade creates opencode_usage_step_removals")
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_opencode_usage_step_removals_removed'`,
	).Scan(&have))
	assert.Equal(t, 1, have, "upgrade creates the removed_at index")
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 153, ver, "version advanced")

	mustExec(t, d, `INSERT INTO opencode_usage_step_removals
		(conv_id, message_id, removed_at) VALUES ('ses-a', 'msg-a', '2026-07-24T00:00:00Z')`)
	require.NoError(t, migrateV152toV153(d), "re-run is a clean no-op")
	var rows int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM opencode_usage_step_removals`).Scan(&rows))
	assert.Equal(t, 1, rows, "repeated migration preserves existing rows")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 153, ver, "repeated migration keeps the version at 153")
}
