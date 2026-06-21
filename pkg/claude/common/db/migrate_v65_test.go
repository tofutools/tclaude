package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV64toV65_AddsRemoteControl seeds a bare v64 sessions table, runs
// the v65 migration, and asserts the `remote_control` column lands with its
// default 0 (so a pre-v65 row reads as remote-control off) and accepts a write.
func TestMigrateV64toV65_AddsRemoteControl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v64.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (64);
		CREATE TABLE sessions (id TEXT PRIMARY KEY);
		INSERT INTO sessions (id) VALUES ('sess-1');
	`)
	require.NoError(t, err, "seed v64 schema")

	require.NoError(t, migrateV64toV65(d), "migrateV64toV65")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 65, ver, "schema_version after migration")

	// Existing row defaults to 0 (remote control off).
	var rc int
	require.NoError(t, d.QueryRow(`SELECT remote_control FROM sessions WHERE id = 'sess-1'`).Scan(&rc))
	assert.Equal(t, 0, rc, "pre-v65 row defaults to remote_control off")

	_, err = d.Exec(`UPDATE sessions SET remote_control = 1 WHERE id = 'sess-1'`)
	require.NoError(t, err, "remote_control accepts writes")
}

// TestMigrateV64toV65_HealsHalfAppliedRun guards the wedge class the v54
// migration first hit: an interrupted earlier attempt added the column but
// never bumped schema_version. The pragma_table_info probe makes the re-run
// skip the duplicate ALTER and land on 65 — existing data survives — and a
// second re-run is a clean no-op.
func TestMigrateV64toV65_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v64-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: sessions already has remote_control (with a non-default
	// value, to prove the re-run preserves it), version still 64.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (64);
		CREATE TABLE sessions (
			id             TEXT PRIMARY KEY,
			remote_control INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO sessions (id, remote_control) VALUES ('sess-1', 1);
	`)
	require.NoError(t, err, "seed half-applied v64 schema")

	require.NoError(t, migrateV64toV65(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 65, ver, "schema_version finally lands on 65")

	var rc int
	require.NoError(t, d.QueryRow(`SELECT remote_control FROM sessions WHERE id = 'sess-1'`).Scan(&rc))
	assert.Equal(t, 1, rc, "existing remote_control value survives the healing run")

	// Second re-run: the probe finds the column, the ALTER skips, version
	// stays 65.
	require.NoError(t, migrateV64toV65(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 65, ver, "second re-run stays at 65")
}

// TestMigrateV64toV65_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts sessions has the remote_control column. v65 is head, so
// this is where the literal currentVersion pin now lives — the tripwire the
// next migration's author moves forward into their own v66 test.
func TestMigrateV64toV65_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire moved forward into the v66 test
	// (TestMigrateV65toV66_FreshSchema) when JOH-262 added v66.

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'remote_control'`,
	).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "fresh schema has sessions.remote_control")
}

// TestSetSessionRemoteControl_SurvivesSaveSessionTick is the durability guard
// for the out-of-band discipline: remote_control is set by its own targeted
// UPDATE, and SaveSession (the UPSERT a status-tracking hook fires on every
// tick, building a fresh SessionRow that never sets RemoteControl) must NOT
// reset it. This is the exact clobber the context-window columns dodge; if a
// future change folds remote_control into SaveSession's UPSERT, this fails.
func TestSetSessionRemoteControl_SurvivesSaveSessionTick(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "c1", Status: "running"}))

	// Default off until armed.
	got, err := LoadSession("s1")
	require.NoError(t, err)
	assert.False(t, got.RemoteControl, "remote control defaults off")

	require.NoError(t, SetSessionRemoteControl("s1", true), "arm remote control")
	got, err = LoadSession("s1")
	require.NoError(t, err)
	assert.True(t, got.RemoteControl, "remote control reads back armed")

	// A hook tick: a FRESH SessionRow (RemoteControl unset → false) upserted
	// by SaveSession must leave the armed column untouched.
	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "c1", Status: "idle"}))
	again, err := LoadSession("s1")
	require.NoError(t, err)
	assert.True(t, again.RemoteControl, "remote control survives a SaveSession hook tick")
	assert.Equal(t, "idle", again.Status, "the tick's own columns still update")

	// Disarming round-trips too.
	require.NoError(t, SetSessionRemoteControl("s1", false), "disarm remote control")
	off, err := LoadSession("s1")
	require.NoError(t, err)
	assert.False(t, off.RemoteControl, "remote control reads back disarmed")
}

// TestRemoteControlForConv reads the best-known state via the conv-keyed
// convenience reader the CLI status verb + dashboard payload use.
func TestRemoteControlForConv(t *testing.T) {
	setupTestDB(t)

	// No session row for the conv → false, no error.
	on, err := RemoteControlForConv("missing")
	require.NoError(t, err)
	assert.False(t, on, "unknown conv reads as off")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "c1", Status: "running"}))
	on, err = RemoteControlForConv("c1")
	require.NoError(t, err)
	assert.False(t, on, "fresh session reads as off")

	require.NoError(t, SetSessionRemoteControl("s1", true))
	on, err = RemoteControlForConv("c1")
	require.NoError(t, err)
	assert.True(t, on, "armed session reads as on by conv id")
}
