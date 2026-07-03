package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The FreshSchema + currentVersion-tripwire test moved forward to the head
// migration's file (migrate_v88_work_pattern_test.go), per convention.

// TestMigrateV86toV87_AddsColumn drives the real v86→v87 ALTER over a
// v86-pinned DB: it asserts sessions.subagents_json appears, that a
// pre-existing row reads back as "" (no ledger yet — the read side falls back
// to the raw subagent_count for such rows), that the version advances, and
// that a re-run is a clean no-op.
func TestMigrateV86toV87_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v86 and drop the new column so we re-add it from a true v86
	// shape (the fresh chain already ran v87). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN subagents_json`)
	mustExec(t, d, `UPDATE schema_version SET version = 86`)

	// A pre-existing session row (without the new column) must survive the
	// ALTER and read back with the default.
	mustExec(t, d, `INSERT INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		 auto_registered, created_at, updated_at)
		VALUES ('legacy-sess', 'tmux-legacy', 1234, '/tmp', 'conv-legacy', 'idle', '', 2,
		 0, '2026-07-02T00:00:00Z', '2026-07-02T00:00:00Z')`)

	require.NoError(t, migrateV86toV87(d), "v86→v87")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'subagents_json'`).Scan(&n))
	assert.Equal(t, 1, n, "sessions.subagents_json added")

	var ledger string
	require.NoError(t, d.QueryRow(
		`SELECT subagents_json FROM sessions WHERE id = 'legacy-sess'`).Scan(&ledger))
	assert.Equal(t, "", ledger, "existing row defaults to no-ledger-yet")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 87, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV86toV87(d), "v86→v87 re-run is a clean no-op")
}
