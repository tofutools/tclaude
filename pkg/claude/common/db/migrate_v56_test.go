package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV55toV56_AddsHarnessColumns seeds bare v55 sessions +
// conv_index tables, runs the v56 migration, and asserts the `harness`
// column lands on BOTH: existing rows default to 'claude' (so every
// pre-v56 row keeps resolving to Claude Code) and the column accepts
// writes.
func TestMigrateV55toV56_AddsHarnessColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v55.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (55);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY
		);
		CREATE TABLE conv_index (
			conv_id TEXT PRIMARY KEY
		);
		INSERT INTO sessions (id) VALUES ('sess-1');
		INSERT INTO conv_index (conv_id) VALUES ('conv-1');
	`)
	require.NoError(t, err, "seed v55 schema")

	require.NoError(t, migrateV55toV56(d), "migrateV55toV56")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 56, ver, "schema_version after migration")

	var sessHarness, convHarness string
	require.NoError(t, d.QueryRow(`SELECT harness FROM sessions WHERE id = 'sess-1'`).Scan(&sessHarness))
	require.NoError(t, d.QueryRow(`SELECT harness FROM conv_index WHERE conv_id = 'conv-1'`).Scan(&convHarness))
	assert.Equal(t, "claude", sessHarness, "existing session rows default to claude")
	assert.Equal(t, "claude", convHarness, "existing conv_index rows default to claude")

	_, err = d.Exec(`UPDATE conv_index SET harness = 'codex' WHERE conv_id = 'conv-1'`)
	require.NoError(t, err, "harness accepts writes")
}

// TestMigrateV55toV56_HealsHalfAppliedRun guards the same wedge class the
// v54 migration first hit. The v56 migration adds the column to two
// tables, so the interesting half-applied state is "one table got it, the
// other didn't, version still 55". Probing each table independently must
// make the re-run converge: skip the table that already has the column,
// add it to the one that doesn't, land on 56 — without resetting the
// already-populated column.
func TestMigrateV55toV56_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v55-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// sessions already has harness (with a non-default value, to prove the
	// re-run doesn't recreate/reset it); conv_index does not; version 55.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (55);
		CREATE TABLE sessions (
			id      TEXT PRIMARY KEY,
			harness TEXT NOT NULL DEFAULT 'claude'
		);
		CREATE TABLE conv_index (
			conv_id TEXT PRIMARY KEY
		);
		INSERT INTO sessions (id, harness) VALUES ('sess-1', 'codex');
		INSERT INTO conv_index (conv_id) VALUES ('conv-1');
	`)
	require.NoError(t, err, "seed half-applied v55 schema")

	require.NoError(t, migrateV55toV56(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 56, ver, "schema_version finally lands on 56")

	var sessHarness string
	require.NoError(t, d.QueryRow(`SELECT harness FROM sessions WHERE id = 'sess-1'`).Scan(&sessHarness))
	assert.Equal(t, "codex", sessHarness, "existing column data survives the healing run")

	// conv_index got the column on this run, defaulting existing rows.
	var convHarness string
	require.NoError(t, d.QueryRow(`SELECT harness FROM conv_index WHERE conv_id = 'conv-1'`).Scan(&convHarness))
	assert.Equal(t, "claude", convHarness, "the missing column was added with the default")

	// A second run on the now-complete schema is a converging no-op: both
	// probes find the column, both ALTERs skip, version stays 56.
	require.NoError(t, migrateV55toV56(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 56, ver, "second re-run stays at 56")
}

// TestMigrateV55toV56_FreshSchemaRoundTrips builds a fresh DB through the
// full migrate() chain and round-trips harness through the production
// SaveSession / UpsertConvIndex / read helpers. Carries the literal
// currentVersion pin — the tripwire the next migration's author moves
// forward into their own v57 test.
func TestMigrateV55toV56_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 56, currentVersion, "currentVersion is 56")

	// A session saved without a harness defaults to claude (the empty →
	// DefaultHarness coalescing in SaveSession + the column default).
	require.NoError(t, SaveSession(&SessionRow{ID: "sess-1", TmuxSession: "t1", Status: "running"}))
	got, err := LoadSession("sess-1")
	require.NoError(t, err, "LoadSession")
	assert.Equal(t, DefaultHarness, got.Harness, "default-harness session round-trips as claude")

	// An explicit harness round-trips verbatim — the path the Codex spawn
	// path will use.
	require.NoError(t, SaveSession(&SessionRow{ID: "sess-2", TmuxSession: "t2", Status: "running", Harness: "codex"}))
	got2, err := LoadSession("sess-2")
	require.NoError(t, err, "LoadSession codex")
	assert.Equal(t, "codex", got2.Harness, "explicit harness round-trips")

	// conv_index: empty harness on a fresh scan coalesces to claude.
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: "conv-1", ProjectDir: "/p", Created: "2026-01-01T00:00:00Z",
	}))
	row, err := GetConvIndex("conv-1")
	require.NoError(t, err, "GetConvIndex")
	require.NotNil(t, row)
	assert.Equal(t, DefaultHarness, row.Harness, "default-harness conv round-trips as claude")

	// And an explicit codex conv survives a rescan upsert (self-healing).
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: "conv-2", ProjectDir: "/p", Created: "2026-01-01T00:00:00Z", Harness: "codex",
	}))
	row2, err := GetConvIndex("conv-2")
	require.NoError(t, err, "GetConvIndex codex")
	require.NotNil(t, row2)
	assert.Equal(t, "codex", row2.Harness, "explicit conv harness round-trips")
}
