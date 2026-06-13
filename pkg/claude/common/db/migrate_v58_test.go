package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV57toV58_AddsSandboxColumn seeds a bare v57 sessions table,
// runs the v58 migration, and asserts the `sandbox_mode` column lands with
// its empty default (so a pre-v58 row shows no sandbox badge) and accepts a
// write.
func TestMigrateV57toV58_AddsSandboxColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v57.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (57);
		CREATE TABLE sessions (id TEXT PRIMARY KEY);
		INSERT INTO sessions (id) VALUES ('sess-1');
	`)
	require.NoError(t, err, "seed v57 schema")

	require.NoError(t, migrateV57toV58(d), "migrateV57toV58")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 58, ver, "schema_version after migration")

	// Existing row defaults to "" (no sandbox), the harness-with-no-launch-
	// sandbox value (Claude Code).
	var mode string
	require.NoError(t, d.QueryRow(`SELECT sandbox_mode FROM sessions WHERE id = 'sess-1'`).Scan(&mode))
	assert.Equal(t, "", mode, "pre-v58 row defaults to empty sandbox_mode")

	_, err = d.Exec(`UPDATE sessions SET sandbox_mode = 'workspace-write' WHERE id = 'sess-1'`)
	require.NoError(t, err, "sandbox_mode accepts writes")
}

// TestMigrateV57toV58_HealsHalfAppliedRun guards the wedge class the v54
// migration first hit: an interrupted earlier attempt added the column but
// never bumped schema_version. The pragma_table_info probe makes the re-run
// skip the duplicate ALTER and land on 58 — existing data survives — and a
// second re-run is a clean no-op.
func TestMigrateV57toV58_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v57-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: sessions already has sandbox_mode (with a non-default
	// value, to prove the re-run preserves it), version still 57.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (57);
		CREATE TABLE sessions (
			id           TEXT PRIMARY KEY,
			sandbox_mode TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO sessions (id, sandbox_mode) VALUES ('sess-1', 'read-only');
	`)
	require.NoError(t, err, "seed half-applied v57 schema")

	require.NoError(t, migrateV57toV58(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 58, ver, "schema_version finally lands on 58")

	var mode string
	require.NoError(t, d.QueryRow(`SELECT sandbox_mode FROM sessions WHERE id = 'sess-1'`).Scan(&mode))
	assert.Equal(t, "read-only", mode, "existing sandbox_mode value survives the healing run")

	// Second re-run: the probe finds the column, the ALTER skips, version
	// stays 58.
	require.NoError(t, migrateV57toV58(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 58, ver, "second re-run stays at 58")
}

// TestMigrateV57toV58_FreshSchemaRoundTrips builds a fresh DB through the
// full migrate() chain and round-trips sandbox_mode through the production
// SaveSession / LoadSession helpers. Carries the literal currentVersion pin
// — the tripwire the next migration's author moves forward into their own
// v59 test.
func TestMigrateV57toV58_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 58, currentVersion, "currentVersion is 58")

	// A session saved without a sandbox mode round-trips as "" (no sandbox
	// — Claude Code, or a Codex session launched outside the daemon spawn
	// path). Unlike harness, "" is NOT coalesced to a default.
	require.NoError(t, SaveSession(&SessionRow{ID: "sess-1", TmuxSession: "t1", Status: "running"}))
	got, err := LoadSession("sess-1")
	require.NoError(t, err, "LoadSession")
	assert.Equal(t, "", got.SandboxMode, "no-sandbox session round-trips as empty")

	// An explicit sandbox mode round-trips verbatim — the path the daemon /
	// `session new` Codex spawn uses.
	require.NoError(t, SaveSession(&SessionRow{ID: "sess-2", TmuxSession: "t2", Status: "running", Harness: "codex", SandboxMode: "workspace-write"}))
	got2, err := LoadSession("sess-2")
	require.NoError(t, err, "LoadSession codex")
	assert.Equal(t, "workspace-write", got2.SandboxMode, "explicit sandbox mode round-trips")
	assert.Equal(t, "codex", got2.Harness, "harness still round-trips alongside")
}

// TestSaveSession_SandboxModeSurvivesLoadMutateSave is the durability guard
// for the sessions side: the sandbox mode set at spawn must survive the
// load→mutate→save cycle the hook callback runs on every status tick (the
// hook supplies no sandbox of its own, so the value rides through on the
// loaded row — the same pattern harness relies on).
func TestSaveSession_SandboxModeSurvivesLoadMutateSave(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "c1", Status: "running", Harness: "codex", SandboxMode: "danger-full-access"}))

	// Load → mutate → save, the hook-tick pattern.
	got, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "danger-full-access", got.SandboxMode, "load reads the sandbox mode back")
	got.Status = "idle"
	require.NoError(t, SaveSession(got))

	again, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "danger-full-access", again.SandboxMode, "sandbox mode survives a load→mutate→save cycle")
	assert.Equal(t, "idle", again.Status)
}
