package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV87toV88_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v88 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v87).
func TestMigrateV87toV88_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 88, currentVersion, "tripwire: bump this and add a v88→v89 test when you add a migration")
}

// TestMigrateV87toV88_AddsColumn drives the real v87→v88 ALTER over a
// v87-pinned DB: it asserts group_templates.work_pattern appears, that a
// pre-existing template reads back as '' (no pattern), that the version
// advances, and that a re-run is a clean no-op.
func TestMigrateV87toV88_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v87 and drop the new column so we re-add it from a true v87
	// shape (the fresh chain already ran v88). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE group_templates DROP COLUMN work_pattern`)
	mustExec(t, d, `UPDATE schema_version SET version = 87`)

	// A pre-existing template (without the new column) must survive the
	// ALTER and read back with the default.
	mustExec(t, d, `INSERT INTO group_templates
		(name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', 'ctx', '2026-07-03T00:00:00Z', '2026-07-03T00:00:00Z')`)

	require.NoError(t, migrateV87toV88(d), "v87→v88")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_templates') WHERE name = 'work_pattern'`).Scan(&n))
	assert.Equal(t, 1, n, "group_templates.work_pattern added")

	var wp string
	require.NoError(t, d.QueryRow(
		`SELECT work_pattern FROM group_templates WHERE name = 'legacy'`).Scan(&wp))
	assert.Equal(t, "", wp, "existing template defaults to no pattern")
	assert.Empty(t, workPatternFromJSON(wp), "'' reads back as an empty pattern")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 88, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV87toV88(d), "v87→v88 re-run is a clean no-op")
}

// TestMigrateV87toV88_HealsTaskForcesStampedV87 replays the parallel-v87
// history this migration exists to reconcile: a DB stamped 87 by a
// task-forces build has group_templates.work_pattern but NOT
// sessions.subagents_json (it never ran main's v86→v87). v87→v88 must add
// the missing subagents_json while its work_pattern guard no-ops.
func TestMigrateV87toV88_HealsTaskForcesStampedV87(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Shape the task-forces-stamped v87 DB: work_pattern present (schema.sql
	// already has it), subagents_json missing.
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN subagents_json`)
	mustExec(t, d, `UPDATE schema_version SET version = 87`)

	require.NoError(t, migrateV87toV88(d), "v87→v88 over a task-forces-stamped DB")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'subagents_json'`).Scan(&n))
	assert.Equal(t, 1, n, "sessions.subagents_json healed")
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_templates') WHERE name = 'work_pattern'`).Scan(&n))
	assert.Equal(t, 1, n, "work_pattern still present (guard no-oped)")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 88, ver, "version advanced")
}
