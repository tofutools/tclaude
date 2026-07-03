package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV85toV86_AddsColumn drives the real v85→v86 ALTER over a v85-pinned
// DB: it asserts agent_cron_jobs.cron_expr appears, that an existing interval
// job reads back as "" (interval mode), that the version advances, and that a
// re-run is a clean no-op.
func TestMigrateV85toV86_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v85 and drop the new column so we re-add it from a true v85
	// shape (the fresh chain already ran v86). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE agent_cron_jobs DROP COLUMN cron_expr`)
	mustExec(t, d, `UPDATE schema_version SET version = 85`)

	// A pre-existing interval job (without the new column) must survive the
	// ALTER and read back with the default.
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_agent, target_kind, target_agent, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES ('legacy', 'agt_owner', 'conv', 'agt_target', 0, 600,
		 's', 'b', 1, '2026-07-02T00:00:00Z', '', '')`)

	require.NoError(t, migrateV85toV86(d), "v85→v86")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_cron_jobs') WHERE name = 'cron_expr'`).Scan(&n))
	assert.Equal(t, 1, n, "agent_cron_jobs.cron_expr added")

	var expr string
	require.NoError(t, d.QueryRow(
		`SELECT cron_expr FROM agent_cron_jobs WHERE name = 'legacy'`).Scan(&expr))
	assert.Equal(t, "", expr, "existing row defaults to interval mode")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 86, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV85toV86(d), "v85→v86 re-run is a clean no-op")
}
