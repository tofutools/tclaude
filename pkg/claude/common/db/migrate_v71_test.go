package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV70toV71_AddsModelColumnAndBackfills seeds a bare v70 DB
// (session_cost_daily as it stands at v70, plus a sessions table), runs
// the v71 migration, and asserts the model column lands and is backfilled
// from the live sessions row where one still carries a model. The row
// whose session is already gone keeps the empty default.
func TestMigrateV70toV71_AddsModelColumnAndBackfills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v70.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (70);

		CREATE TABLE session_cost_daily (
			session_id       TEXT NOT NULL,
			day              TEXT NOT NULL,
			conv_id          TEXT NOT NULL DEFAULT '',
			cost_usd         REAL NOT NULL DEFAULT 0,
			virtual_cost_usd REAL NOT NULL DEFAULT 0,
			updated_at       TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (session_id, day)
		);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT NOT NULL DEFAULT '');

		-- alive: a sessions row still carries a model → backfilled.
		INSERT INTO sessions (id, model) VALUES ('alive-1', 'Opus 4.8 (1M context)');
		INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('alive-1', '2026-06-20', 'conv-alive', 1.00);
		-- retired: the daily row survives but its sessions row is gone →
		-- the model stays empty, the pre-v71 history this can't recover.
		INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('retired-1', '2026-06-19', 'conv-retired', 2.00);
	`)
	require.NoError(t, err, "seed v70 schema")

	require.NoError(t, migrateV70toV71(d), "migrateV70toV71")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 71, ver, "schema_version after migration")

	var aliveModel, retiredModel string
	require.NoError(t, d.QueryRow(
		`SELECT model FROM session_cost_daily WHERE session_id = 'alive-1'`).Scan(&aliveModel))
	require.NoError(t, d.QueryRow(
		`SELECT model FROM session_cost_daily WHERE session_id = 'retired-1'`).Scan(&retiredModel))
	assert.Equal(t, "Opus 4.8 (1M context)", aliveModel, "backfilled from the live sessions row")
	assert.Empty(t, retiredModel, "no live session → model can't be recovered, stays empty")

	// Second run is a no-op (column already present) and stays on 71.
	require.NoError(t, migrateV70toV71(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 71, ver, "second re-run stays at 71")
}

// TestMigrateV70toV71_HealsTablelessSchema covers the partial-schema heal
// path: a synthetic DB seeded for an unrelated migration may not have
// session_cost_daily at all. The migration must no-op the column add and
// just advance the version, never wedge on "no such table".
func TestMigrateV70toV71_HealsTablelessSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v70-tableless.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (70);
	`)
	require.NoError(t, err, "seed tableless v70 schema")

	require.NoError(t, migrateV70toV71(d), "no session_cost_daily → no-op, not an error")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 71, ver, "version advances even with the table absent")
}

// TestMigrateV70toV71_HealsSessionlessSchema covers the inverse partial
// schema: session_cost_daily is present but sessions is not. The column
// add must still land, and the backfill (which reads sessions) must be
// skipped rather than wedge on "no such table: sessions".
func TestMigrateV70toV71_HealsSessionlessSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v70-sessionless.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (70);
		CREATE TABLE session_cost_daily (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, day)
		);
		INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('s1', '2026-06-20', 'conv-1', 1.00);
	`)
	require.NoError(t, err, "seed sessionless v70 schema")

	require.NoError(t, migrateV70toV71(d), "no sessions table → backfill skipped, not an error")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 71, ver, "version advances")

	var model string
	require.NoError(t, d.QueryRow(`SELECT model FROM session_cost_daily WHERE session_id = 's1'`).Scan(&model))
	assert.Empty(t, model, "column added with the empty default; no sessions to backfill from")
}

// TestMigrateV70toV71_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts session_cost_daily.model exists. v71 is
// head, so this is where the literal currentVersion tripwire now lives —
// the next migration's author moves it forward into their own v72 test.
func TestMigrateV70toV71_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v70 test —
	// the next migration's author moves it into their own v72 test.
	require.Equal(t, 71, currentVersion, "currentVersion is 71")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_cost_daily') WHERE name = 'model'`).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "fresh schema has session_cost_daily.model")
}
