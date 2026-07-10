package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV105toV106_AddsDurableNudgeState(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	for _, col := range []string{"nudge_claimed_at", "nudge_attempted_at", "nudge_attempts"} {
		var have int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = ?`, col,
		).Scan(&have))
		assert.Equal(t, 1, have, "fresh schema has %s", col)
	}

	mustExec(t, d, `ALTER TABLE agent_messages DROP COLUMN nudge_claimed_at`)
	mustExec(t, d, `ALTER TABLE agent_messages DROP COLUMN nudge_attempted_at`)
	mustExec(t, d, `ALTER TABLE agent_messages DROP COLUMN nudge_attempts`)
	mustExec(t, d, `UPDATE schema_version SET version = 105`)
	require.NoError(t, migrateV105toV106(d))
	require.NoError(t, migrateV105toV106(d), "half-applied/re-run converges")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 106, ver)
}
