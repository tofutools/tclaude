package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV84toV85_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v85 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v84).
func TestMigrateV84toV85_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 85, currentVersion, "tripwire: bump this and add a v85→v86 test when you add a migration")
}

// TestMigrateV84toV85_AddsColumn drives the real v84→v85 ALTER over a v84-pinned
// DB: it asserts sessions.last_statusline_json appears, that an existing session
// row reads back as "" (no snapshot yet), that the version advances, and that a
// re-run is a clean no-op.
func TestMigrateV84toV85_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v84 and drop the new column so we re-add it from a true v84
	// shape (the fresh chain already ran v85). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN last_statusline_json`)
	mustExec(t, d, `UPDATE schema_version SET version = 84`)

	// A pre-existing session row (without the new column) must survive the ALTER
	// and read back with the default.
	mustExec(t, d, `INSERT INTO sessions (id, created_at, updated_at)
		VALUES ('sess-existing', '2026-07-02T00:00:00Z', '2026-07-02T00:00:00Z')`)

	require.NoError(t, migrateV84toV85(d), "v84→v85")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'last_statusline_json'`).Scan(&n))
	assert.Equal(t, 1, n, "sessions.last_statusline_json added")

	var snap string
	require.NoError(t, d.QueryRow(
		`SELECT last_statusline_json FROM sessions WHERE id = 'sess-existing'`).Scan(&snap))
	assert.Equal(t, "", snap, "existing row defaults to no snapshot")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 85, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV84toV85(d), "v84→v85 re-run is a clean no-op")
}

// TestUpdateStatuslineSnapshot_RoundTrip exercises the write helper: the verbatim
// JSON it stores reads back byte-for-byte off the row, an empty payload is a
// no-op that never blanks a good snapshot, and an unknown session is a no-op.
func TestUpdateStatuslineSnapshot_RoundTrip(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	mustExec(t, d, `INSERT INTO sessions (id, created_at, updated_at)
		VALUES ('sess-1', '2026-07-02T00:00:00Z', '2026-07-02T00:00:00Z')`)

	// A raw payload carrying a field StatusLineInput doesn't name (fable_bucket)
	// must round-trip verbatim — the whole point of storing raw bytes.
	const raw = `{"rate_limits":{"five_hour":{"used_percentage":12}},"fable_bucket":{"used_percentage":3}}`
	require.NoError(t, UpdateStatuslineSnapshot("sess-1", raw))

	var got string
	require.NoError(t, d.QueryRow(
		`SELECT last_statusline_json FROM sessions WHERE id = 'sess-1'`).Scan(&got))
	assert.Equal(t, raw, got, "stored verbatim")

	// Empty payload is a no-op: the previous snapshot survives.
	require.NoError(t, UpdateStatuslineSnapshot("sess-1", ""))
	require.NoError(t, d.QueryRow(
		`SELECT last_statusline_json FROM sessions WHERE id = 'sess-1'`).Scan(&got))
	assert.Equal(t, raw, got, "empty payload does not blank a good snapshot")

	// Unknown session is a no-op, not an error.
	require.NoError(t, UpdateStatuslineSnapshot("sess-nope", raw))
}
