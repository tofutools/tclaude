package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV86toV87_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v87 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v86).
func TestMigrateV86toV87_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 87, currentVersion, "tripwire: bump this and add a v87→v88 test when you add a migration")
}

// TestMigrateV86toV87_AddsColumn drives the real v86→v87 ALTER over a
// v86-pinned DB: it asserts group_templates.work_pattern appears, that a
// pre-existing template reads back as '' (no pattern), that the version
// advances, and that a re-run is a clean no-op.
func TestMigrateV86toV87_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v86 and drop the new column so we re-add it from a true v86
	// shape (the fresh chain already ran v87). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE group_templates DROP COLUMN work_pattern`)
	mustExec(t, d, `UPDATE schema_version SET version = 86`)

	// A pre-existing template (without the new column) must survive the
	// ALTER and read back with the default.
	mustExec(t, d, `INSERT INTO group_templates
		(name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', 'ctx', '2026-07-03T00:00:00Z', '2026-07-03T00:00:00Z')`)

	require.NoError(t, migrateV86toV87(d), "v86→v87")

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
	assert.Equal(t, 87, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV86toV87(d), "v86→v87 re-run is a clean no-op")
}
