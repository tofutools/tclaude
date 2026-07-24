package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV153toV154AddsCronOperatorAuthoredDisabledForLegacyJobs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE agent_cron_jobs DROP COLUMN operator_authored`)
	mustExec(t, d, `UPDATE schema_version SET version = 153`)
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_agent, target_agent, interval_seconds, created_at)
		VALUES ('legacy-job', '', '', 600, '2026-07-24T09:00:00Z')`)

	require.NoError(t, migrateV153toV154(d))
	var operatorAuthored int
	require.NoError(t, d.QueryRow(
		`SELECT operator_authored FROM agent_cron_jobs WHERE name = 'legacy-job'`,
	).Scan(&operatorAuthored))
	assert.Zero(t, operatorAuthored, "legacy jobs keep their existing agent/owner attribution")
	assert.Equal(t, 154, schemaVersion(d))
	require.NoError(t, migrateV153toV154(d), "partially applied migration converges")
}

func TestMigrateV154IsTheCurrentHead(t *testing.T) {
	require.Equal(t, 154, currentVersion, "tripwire: bump this with the next migration")
}
