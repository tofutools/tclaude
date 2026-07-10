package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV107toV108_AddsNudgeCancellation(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	for _, col := range []string{"nudge_cancelled_at", "nudge_cancel_reason"} {
		var have int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = ?`, col,
		).Scan(&have))
		assert.Equal(t, 1, have, "fresh schema has %s", col)
	}

	mustExec(t, d, `ALTER TABLE agent_messages DROP COLUMN nudge_cancelled_at`)
	mustExec(t, d, `ALTER TABLE agent_messages DROP COLUMN nudge_cancel_reason`)
	mustExec(t, d, `UPDATE schema_version SET version = 107`)
	require.NoError(t, migrateV107toV108(d))
	require.NoError(t, migrateV107toV108(d), "half-applied/re-run converges")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 108, ver)
}
