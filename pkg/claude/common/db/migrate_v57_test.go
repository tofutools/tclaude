package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV56toV57_AddsHarnessColumns seeds bare v56 sessions +
// conv_index tables, runs the v57 migration, and asserts the `harness`
// column lands on BOTH: existing rows default to 'claude' (so every
// pre-v57 row keeps resolving to Claude Code) and the column accepts
// writes.
func TestMigrateV56toV57_AddsHarnessColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v56.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (56);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY
		);
		CREATE TABLE conv_index (
			conv_id TEXT PRIMARY KEY
		);
		INSERT INTO sessions (id) VALUES ('sess-1');
		INSERT INTO conv_index (conv_id) VALUES ('conv-1');
	`)
	require.NoError(t, err, "seed v56 schema")

	require.NoError(t, migrateV56toV57(d), "migrateV56toV57")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 57, ver, "schema_version after migration")

	var sessHarness, convHarness string
	require.NoError(t, d.QueryRow(`SELECT harness FROM sessions WHERE id = 'sess-1'`).Scan(&sessHarness))
	require.NoError(t, d.QueryRow(`SELECT harness FROM conv_index WHERE conv_id = 'conv-1'`).Scan(&convHarness))
	assert.Equal(t, "claude", sessHarness, "existing session rows default to claude")
	assert.Equal(t, "claude", convHarness, "existing conv_index rows default to claude")

	_, err = d.Exec(`UPDATE conv_index SET harness = 'codex' WHERE conv_id = 'conv-1'`)
	require.NoError(t, err, "harness accepts writes")
}

// TestMigrateV56toV57_HealsHalfAppliedRun guards the same wedge class the
// v54 migration first hit. The v57 migration adds the column to two
// tables, so the interesting half-applied state is "one table got it, the
// other didn't, version still 56". Probing each table independently must
// make the re-run converge: skip the table that already has the column,
// add it to the one that doesn't, land on 57 — without resetting the
// already-populated column.
func TestMigrateV56toV57_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v56-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// sessions already has harness (with a non-default value, to prove the
	// re-run doesn't recreate/reset it); conv_index does not; version 56.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (56);
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
	require.NoError(t, err, "seed half-applied v56 schema")

	require.NoError(t, migrateV56toV57(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 57, ver, "schema_version finally lands on 57")

	var sessHarness string
	require.NoError(t, d.QueryRow(`SELECT harness FROM sessions WHERE id = 'sess-1'`).Scan(&sessHarness))
	assert.Equal(t, "codex", sessHarness, "existing column data survives the healing run")

	// conv_index got the column on this run, defaulting existing rows.
	var convHarness string
	require.NoError(t, d.QueryRow(`SELECT harness FROM conv_index WHERE conv_id = 'conv-1'`).Scan(&convHarness))
	assert.Equal(t, "claude", convHarness, "the missing column was added with the default")

	// A second run on the now-complete schema is a converging no-op: both
	// probes find the column, both ALTERs skip, version stays 57.
	require.NoError(t, migrateV56toV57(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 57, ver, "second re-run stays at 57")
}

// TestMigrateV56toV57_FreshSchemaRoundTrips builds a fresh DB through the
// full migrate() chain and round-trips harness through the production
// SaveSession / UpsertConvIndex / read helpers. The literal currentVersion
// pin moved on to the v58 test (TestMigrateV57toV58_FreshSchemaRoundTrips)
// — the sandbox_mode migration that landed on top of this one; this test
// defers to the head.
func TestMigrateV56toV57_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

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

// TestUpsertConvIndex_HarnessNotClobberedOnRescan is the durability guard
// for the conv_index side: once a conv is tagged (the Codex scanner sets
// 'codex' on INSERT), a later harness-blind rescan — the Claude Code scan
// path builds rows without a harness, which coalesces to 'claude' — must
// NOT overwrite the stored tag. harness is omitted from UpsertConvIndex's
// ON-CONFLICT UPDATE (the archived_at precedent) precisely so this holds.
func TestUpsertConvIndex_HarnessNotClobberedOnRescan(t *testing.T) {
	setupTestDB(t)

	// First write tags the conv 'codex' (what the Codex scanner does).
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: "conv-cod", ProjectDir: "/p", Created: "2026-01-01T00:00:00Z", Harness: "codex",
	}))

	// A later harness-blind rescan upserts the same conv with no harness
	// (→ coalesced to 'claude' on the INSERT branch). The ON-CONFLICT
	// update must leave the stored 'codex' intact.
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: "conv-cod", ProjectDir: "/p", Created: "2026-01-02T00:00:00Z", FirstPrompt: "hi",
	}))

	row, err := GetConvIndex("conv-cod")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "codex", row.Harness, "rescan must not clobber the stored harness tag")
	assert.Equal(t, "hi", row.FirstPrompt, "other columns still update on rescan")
}

// TestSaveSession_HarnessSurvivesLoadMutateSave is the durability guard
// for the sessions side at the DB layer: a 'codex' tag round-trips through
// the load→mutate→save cycle the hook callback runs. (db.SaveSession's
// ON-CONFLICT DOES update harness — sessions, unlike conv_index, want a
// spawn's UPDATE to set it — so durability relies on the caller carrying
// the tag through, which a load supplies.)
func TestSaveSession_HarnessSurvivesLoadMutateSave(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "c1", Status: "running", Harness: "codex"}))

	// Load → mutate → save, the hook-tick pattern.
	got, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "codex", got.Harness, "load reads the tag back")
	got.Status = "idle"
	require.NoError(t, SaveSession(got))

	again, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "codex", again.Harness, "tag survives a load→mutate→save cycle")
	assert.Equal(t, "idle", again.Status)
}
