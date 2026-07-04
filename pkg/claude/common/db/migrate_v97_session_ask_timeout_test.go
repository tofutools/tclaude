package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV96toV97_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v97 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v96).
func TestMigrateV96toV97_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 97, currentVersion, "tripwire: bump this and add a v97→v98 test when you add a migration")
}

// TestMigrateV96toV97_AddsColumn drives the real v96→v97 ALTER over a v96-pinned
// DB: it asserts sessions.ask_user_question_timeout appears, that a pre-existing
// session reads back as "" (nothing to preserve), that the version advances, and
// that a re-run is a clean no-op.
func TestMigrateV96toV97_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v96 and drop the new column so we re-add it from a true v96
	// shape (the fresh chain already ran v97). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN ask_user_question_timeout`)
	mustExec(t, d, `UPDATE schema_version SET version = 96`)

	// A pre-existing session row (without the new column) must survive the ALTER
	// and read back with the default.
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-sess', 'tc-legacy', 123, '/tmp', 'conv-legacy', 'idle', '2026-07-04T00:00:00Z', '2026-07-04T00:00:00Z')`)

	require.NoError(t, migrateV96toV97(d), "v96→v97")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'ask_user_question_timeout'`).Scan(&n))
	assert.Equal(t, 1, n, "sessions.ask_user_question_timeout added")

	var v string
	require.NoError(t, d.QueryRow(
		`SELECT ask_user_question_timeout FROM sessions WHERE id = 'legacy-sess'`).Scan(&v))
	assert.Equal(t, "", v, "existing session defaults to nothing to preserve")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 97, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV96toV97(d), "v96→v97 re-run is a clean no-op")
}
