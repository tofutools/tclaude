package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The FreshSchema + currentVersion-tripwire test moved forward to the head
// migration's file (migrate_v95_agent_task_ref_test.go), per convention.

// TestMigrateV93toV94_AddsDisabledReason drives the real v93→v94 migration over
// a v93-pinned DB: the disabled_reason column appears, a pre-existing job reads
// it back as its zero value, the version advances, and a re-run is a clean
// no-op.
func TestMigrateV93toV94_AddsDisabledReason(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v93: drop the new column so we re-create it from a true v93
	// shape (the fresh chain already ran v94).
	mustExec(t, d, `ALTER TABLE agent_cron_jobs DROP COLUMN disabled_reason`)
	mustExec(t, d, `UPDATE schema_version SET version = 93`)

	// A pre-existing cron job (without the new field) must survive the ALTER.
	mustExec(t, d, `INSERT INTO agent_cron_jobs (name, owner_agent, target_agent, interval_seconds, created_at) VALUES ('legacy-job', 'agt_x', 'agt_y', 600, '2026-07-04T00:00:00Z')`)

	require.NoError(t, migrateV93toV94(d), "v93→v94")

	// The new column exists.
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_cron_jobs') WHERE name = 'disabled_reason'`).Scan(&have))
	assert.Equal(t, 1, have, "agent_cron_jobs.disabled_reason added")

	// The legacy row reads the new field back as its zero value.
	var reason string
	require.NoError(t, d.QueryRow(`SELECT disabled_reason FROM agent_cron_jobs WHERE name = 'legacy-job'`).Scan(&reason))
	assert.Empty(t, reason, "legacy job has no disabled_reason")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 94, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op.
	require.NoError(t, migrateV93toV94(d), "v93→v94 re-run is a clean no-op")
}
