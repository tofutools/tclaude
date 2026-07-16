package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV125toV126AddsCronRunImmediatelyWithoutArmingLegacyJobs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE agent_cron_jobs DROP COLUMN run_immediately`)
	mustExec(t, d, `UPDATE schema_version SET version = 125`)
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_agent, target_agent, interval_seconds, created_at, last_run_at)
		VALUES ('legacy-never-run', '', '', 600, '2026-07-16T09:00:00Z', '')`)

	require.NoError(t, migrateV125toV126(d))
	var immediate int
	require.NoError(t, d.QueryRow(
		`SELECT run_immediately FROM agent_cron_jobs WHERE name = 'legacy-never-run'`,
	).Scan(&immediate))
	assert.Zero(t, immediate, "migration must not arm an immediate replay")
	assert.Equal(t, 126, schemaVersion(d))
	require.NoError(t, migrateV125toV126(d), "partially applied migration converges")
}
