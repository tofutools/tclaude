package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV128toV129AddsCronQueueWhenOfflineDisabledForLegacyJobs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE agent_cron_jobs DROP COLUMN queue_when_offline`)
	mustExec(t, d, `UPDATE schema_version SET version = 128`)
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_agent, target_agent, interval_seconds, created_at)
		VALUES ('legacy-job', '', '', 600, '2026-07-16T09:00:00Z')`)

	require.NoError(t, migrateV128toV129(d))
	var queueOffline int
	require.NoError(t, d.QueryRow(
		`SELECT queue_when_offline FROM agent_cron_jobs WHERE name = 'legacy-job'`,
	).Scan(&queueOffline))
	assert.Zero(t, queueOffline, "legacy jobs must adopt the safe discard-offline default")
	assert.Equal(t, 129, schemaVersion(d))
	require.NoError(t, migrateV128toV129(d), "partially applied migration converges")
}
