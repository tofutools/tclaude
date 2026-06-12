package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV52toV53_AddsUpdatedAtAndBackfills seeds a bare v52
// session_cost_daily alongside a sessions table, runs the v53
// migration, and asserts the new updated_at column lands backfilled
// from the session's clock: last_hook when it carries a real
// timestamp, else updated_at, and '' (date-only) for history whose
// session row is already gone or never had a usable timestamp.
func TestMigrateV52toV53_AddsUpdatedAtAndBackfills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v52.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v52 schema: sessions (with the timestamp columns the
	// backfill reads) and the pre-updated_at session_cost_daily table.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (52);
		CREATE TABLE sessions (
			id         TEXT PRIMARY KEY,
			conv_id    TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			last_hook  TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE session_cost_daily (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, day)
		);
		-- hooked: real last_hook → backfill prefers it.
		INSERT INTO sessions (id, conv_id, updated_at, last_hook) VALUES
			('hooked', 'conv-h', '2026-06-10T08:00:00Z', '2026-06-10T09:30:00Z'),
		-- nohook: zero-time last_hook (a never-hooked session) → fall back to updated_at.
			('nohook', 'conv-n', '2026-06-10T07:15:00Z', '0001-01-01T00:00:00Z');
		INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES
			('hooked', '2026-06-10', 'conv-h', 1.00),
			('nohook', '2026-06-10', 'conv-n', 2.00),
			('gone',   '2026-06-09', 'conv-g', 3.00);
	`)
	require.NoError(t, err, "seed v52 schema")

	require.NoError(t, migrateV52toV53(d), "migrateV52toV53")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 53, ver, "schema_version after migration")

	get := func(sessionID string) string {
		var s string
		require.NoError(t, d.QueryRow(
			`SELECT updated_at FROM session_cost_daily WHERE session_id = ?`, sessionID).Scan(&s))
		return s
	}
	assert.Equal(t, "2026-06-10T09:30:00Z", get("hooked"), "real last_hook wins")
	assert.Equal(t, "2026-06-10T07:15:00Z", get("nohook"), "zero-time last_hook falls back to updated_at")
	assert.Equal(t, "", get("gone"), "history with no surviving session stays date-only")
}

// TestMigrateV52toV53_FreshSchemaRoundTrips builds a fresh DB through
// the full migrate() chain and round-trips the daily row's last
// activity stamp through the production write path. Carries the
// literal currentVersion pin — a tripwire the next migration's author
// moves forward into their own v54 test.
func TestMigrateV52toV53_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "conv-1", Status: "idle"}), "SaveSession")
	require.NoError(t, UpdateSessionCost("s1", 0.42), "UpdateSessionCost on a fresh schema")

	rows, err := AllCostDailyRows()
	require.NoError(t, err, "AllCostDailyRows")
	require.Len(t, rows, 1, "one daily row after one costed tick")
	assert.NotEmpty(t, rows[0].UpdatedAt, "the write path stamps a last-activity time")
}
