package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV91toV92_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v92 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v91).
func TestMigrateV91toV92_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 92, currentVersion, "tripwire: bump this and add a v92→v93 test when you add a migration")
}

// TestMigrateV91toV92_AddsProcess drives the real v91→v92 migration over a
// v91-pinned DB: it asserts the process column appears on group_templates, the
// two process-state tables appear, a pre-existing template reads process back
// as '', the version advances, and a re-run is a clean no-op.
func TestMigrateV91toV92_AddsProcess(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v91: drop the process column and the two state tables so we
	// re-create them from a true v91 shape (the fresh chain already ran v92).
	mustExec(t, d, `ALTER TABLE group_templates DROP COLUMN process`)
	mustExec(t, d, `DROP TABLE IF EXISTS group_process_state`)
	mustExec(t, d, `DROP TABLE IF EXISTS group_process_transitions`)
	mustExec(t, d, `UPDATE schema_version SET version = 91`)

	// A pre-existing template (without process) must survive the ALTER.
	mustExec(t, d, `INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', '', '2026-07-03T00:00:00Z', '2026-07-03T00:00:00Z')`)

	require.NoError(t, migrateV91toV92(d), "v91→v92")

	// process column exists on group_templates.
	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_templates') WHERE name = 'process'`).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "group_templates.process added")

	// Both state tables exist.
	for _, tbl := range []string{"group_process_state", "group_process_transitions"} {
		var have int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, tbl).Scan(&have))
		assert.Equalf(t, 1, have, "%s table created", tbl)
	}

	// The legacy template reads process back as unset.
	tmpl, err := GetGroupTemplate("legacy")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	assert.Empty(t, tmpl.Process, "legacy template has no process")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 92, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op.
	require.NoError(t, migrateV91toV92(d), "v91→v92 re-run is a clean no-op")
}
