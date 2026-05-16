package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV42toV43_AddsExitReason seeds a v42 sessions table with one
// row — a session written before exit_reason existed — runs the v43
// migration, and asserts the new column lands NULLABLE with the
// pre-existing row left at NULL. A pre-migration corpse must NOT be
// retroactively stamped with a crash sentinel.
func TestMigrateV42toV43_AddsExitReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v42.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal pre-v43 sessions table + one row. The v43 migration is a
	// plain ADD COLUMN, so the other session columns are irrelevant to
	// what it does; the fresh-schema test below exercises the real,
	// full-width table built through createSchema.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (42);

		CREATE TABLE sessions (
			id     TEXT PRIMARY KEY,
			status TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO sessions (id, status) VALUES ('legacy-sess', 'exited');
	`)
	require.NoError(t, err, "seed v42 schema")

	require.NoError(t, migrateV42toV43(d), "migrateV42toV43")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 43, ver, "schema_version after migration")

	// The pre-existing row survives; its exit_reason is NULL — "no
	// reason recorded", which the dashboard renders as a plain exit.
	var reason sql.NullString
	require.NoError(t, d.QueryRow(
		`SELECT exit_reason FROM sessions WHERE id = 'legacy-sess'`).Scan(&reason))
	assert.False(t, reason.Valid, "a pre-migration row's exit_reason is NULL")

	// A row can be written with an explicit reason and read back.
	_, err = d.Exec(`INSERT INTO sessions (id, status, exit_reason)
		VALUES ('clean-sess', 'exited', 'logout')`)
	require.NoError(t, err, "insert row with exit_reason")
	require.NoError(t, d.QueryRow(
		`SELECT exit_reason FROM sessions WHERE id = 'clean-sess'`).Scan(&reason))
	assert.True(t, reason.Valid, "an explicit exit_reason reads back")
	assert.Equal(t, "logout", reason.String)
}

// TestMigrateV42toV43_FreshSchemaHasExitReason builds a fresh DB through
// the full migrate() chain and confirms sessions.exit_reason exists and
// defaults to NULL on the real, full-width table — pinning that the v43
// block is wired into the dispatcher, not merely defined.
func TestMigrateV42toV43_FreshSchemaHasExitReason(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 43, currentVersion, "currentVersion is 43")

	// A session inserted without exit_reason has it NULL.
	_, err = d.Exec(`INSERT INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail,
		 subagent_count, auto_registered, created_at, updated_at, last_hook)
		VALUES ('s1', '', 0, '', 'c1', 'idle', '', 0, 0,
		        '2026-05-16T00:00:00Z', '2026-05-16T00:00:00Z', '')`)
	require.NoError(t, err, "insert session without exit_reason")
	var reason sql.NullString
	require.NoError(t, d.QueryRow(
		`SELECT exit_reason FROM sessions WHERE id = 's1'`).Scan(&reason))
	assert.False(t, reason.Valid, "exit_reason defaults to NULL")
}
