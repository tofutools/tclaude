package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV128toV129AddsRegularMessageState(t *testing.T) {
	setupTestDB(t)
	require.Equal(t, 129, currentVersion, "tripwire: bump this with the next migration")
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `DROP INDEX idx_agent_messages_regular_agent_backlog`)
	mustExec(t, d, `DROP INDEX idx_agent_messages_regular_conv_backlog`)
	for _, column := range []string{"regular_send", "started_at", "processed_at", "nudge_discarded_at"} {
		mustExec(t, d, `ALTER TABLE agent_messages DROP COLUMN `+column)
	}
	mustExec(t, d, `UPDATE schema_version SET version = 128`)

	require.NoError(t, migrateV128toV129(d))
	require.NoError(t, migrateV128toV129(d), "half-applied/re-run converges")

	columns := tableColumns(t, d, "agent_messages")
	for _, column := range []string{"regular_send", "started_at", "processed_at", "nudge_discarded_at"} {
		assert.Contains(t, columns, column)
	}
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 129, version)
}
