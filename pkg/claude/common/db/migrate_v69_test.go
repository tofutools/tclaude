package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV68toV69_AddsVirtualCost seeds a bare v68 schema, runs the v69
// migration, and asserts the virtual_cost_usd column lands on BOTH sessions and
// session_cost_daily as REAL NOT NULL DEFAULT 0 — so a pre-existing row reads as
// zero virtual cost (nothing captured it yet) and the column accepts a real write.
func TestMigrateV68toV69_AddsVirtualCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v68.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (68);
		CREATE TABLE sessions (
			id       TEXT PRIMARY KEY,
			conv_id  TEXT NOT NULL DEFAULT '',
			cost_usd REAL NOT NULL DEFAULT 0
		);
		INSERT INTO sessions (id, conv_id, cost_usd) VALUES ('s1', 'c1', 0);
		CREATE TABLE session_cost_daily (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (session_id, day)
		);
		INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('s1', '2026-06-10', 'c1', 0);
	`)
	require.NoError(t, err, "seed v68 schema")

	require.NoError(t, migrateV68toV69(d), "migrateV68toV69")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 69, ver, "schema_version after migration")

	// Pre-existing rows read 0 (the NOT NULL DEFAULT), not NULL.
	var sv, dv float64
	require.NoError(t, d.QueryRow(`SELECT virtual_cost_usd FROM sessions WHERE id = 's1'`).Scan(&sv))
	assert.Equal(t, 0.0, sv, "pre-v69 sessions row reads virtual_cost_usd = 0")
	require.NoError(t, d.QueryRow(`SELECT virtual_cost_usd FROM session_cost_daily WHERE session_id = 's1'`).Scan(&dv))
	assert.Equal(t, 0.0, dv, "pre-v69 session_cost_daily row reads virtual_cost_usd = 0")

	// Both columns accept a real write.
	_, err = d.Exec(`UPDATE sessions SET virtual_cost_usd = 1.5 WHERE id = 's1'`)
	require.NoError(t, err, "sessions.virtual_cost_usd accepts a value")
	_, err = d.Exec(`UPDATE session_cost_daily SET virtual_cost_usd = 2.5 WHERE session_id = 's1'`)
	require.NoError(t, err, "session_cost_daily.virtual_cost_usd accepts a value")
}

// TestMigrateV68toV69_HealsHalfAppliedRun guards the wedge class: an interrupted
// earlier attempt added the column on ONE table but never bumped schema_version.
// The per-column pragma_table_info probe makes the re-run skip the already-added
// column, add the missing one, land on 69, and a second re-run is a no-op.
func TestMigrateV68toV69_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v68-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: sessions already has the column (with a non-default value,
	// to prove the re-run preserves it); session_cost_daily does not; version 68.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (68);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, conv_id TEXT NOT NULL DEFAULT '', virtual_cost_usd REAL NOT NULL DEFAULT 0);
		INSERT INTO sessions (id, conv_id, virtual_cost_usd) VALUES ('s1', 'c1', 3.25);
		CREATE TABLE session_cost_daily (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (session_id, day)
		);
	`)
	require.NoError(t, err, "seed half-applied v68 schema")

	require.NoError(t, migrateV68toV69(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 69, ver, "schema_version finally lands on 69")

	// The already-present value survived, and the missing column was added.
	var sv float64
	require.NoError(t, d.QueryRow(`SELECT virtual_cost_usd FROM sessions WHERE id = 's1'`).Scan(&sv))
	assert.Equal(t, 3.25, sv, "existing sessions.virtual_cost_usd survives")
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_cost_daily') WHERE name = 'virtual_cost_usd'`).Scan(&have))
	assert.Equal(t, 1, have, "the missing session_cost_daily.virtual_cost_usd was added")

	// Second re-run: both probes find the columns, both ALTERs skip, stays 69.
	require.NoError(t, migrateV68toV69(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 69, ver, "second re-run stays at 69")
}

// TestMigrateV68toV69_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts both virtual_cost_usd columns exist. v69 is head, so this is
// where the literal currentVersion pin now lives — the tripwire the next
// migration's author moves forward into their own v70 test.
func TestMigrateV68toV69_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v68 test —
	// the next migration's author moves it into their own v70 test.
	require.Equal(t, 69, currentVersion, "currentVersion is 69")

	for _, table := range []string{"sessions", "session_cost_daily"} {
		var haveCol int
		require.NoErrorf(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = 'virtual_cost_usd'`,
		).Scan(&haveCol), "probe %s.virtual_cost_usd", table)
		assert.Equalf(t, 1, haveCol, "fresh schema has %s.virtual_cost_usd", table)
	}
}
