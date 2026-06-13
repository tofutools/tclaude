package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV54toV55_AddsModelID seeds a bare v54 sessions table, runs
// the v55 migration, and asserts sessions.model_id lands: existing rows
// default to '' ("not reported yet" — successor spawns omit --model,
// the pre-v55 behaviour) and the column accepts writes.
func TestMigrateV54toV55_AddsModelID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v54.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (54);
		CREATE TABLE sessions (
			id    TEXT PRIMARY KEY,
			model TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO sessions (id, model) VALUES ('sess-1', 'Fable 5');
	`)
	require.NoError(t, err, "seed v54 schema")

	require.NoError(t, migrateV54toV55(d), "migrateV54toV55")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 55, ver, "schema_version after migration")

	var modelID string
	require.NoError(t, d.QueryRow(`SELECT model_id FROM sessions WHERE id = 'sess-1'`).Scan(&modelID))
	assert.Equal(t, "", modelID, "existing rows default to no recorded model id")

	_, err = d.Exec(`UPDATE sessions SET model_id = 'claude-fable-5' WHERE id = 'sess-1'`)
	require.NoError(t, err, "model_id accepts writes")
}

// TestMigrateV54toV55_HealsHalfAppliedRun guards the wedge class the
// v54 migration first hit: an interrupted earlier attempt added the
// column but never bumped schema_version, so a bare re-run would die
// on "duplicate column name" forever. The pragma_table_info probe must
// make the re-run converge — skip the ALTER, keep existing column
// data, land on version 55.
func TestMigrateV54toV55_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v54-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// The half-applied state: column already there (with a non-default
	// value to prove the re-run doesn't recreate/reset anything),
	// version still 54.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (54);
		CREATE TABLE sessions (
			id       TEXT PRIMARY KEY,
			model    TEXT NOT NULL DEFAULT '',
			model_id TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO sessions (id, model, model_id) VALUES ('sess-1', 'Fable 5', 'claude-fable-5');
	`)
	require.NoError(t, err, "seed half-applied v54 schema")

	require.NoError(t, migrateV54toV55(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 55, ver, "schema_version finally lands on 55")

	var modelID string
	require.NoError(t, d.QueryRow(`SELECT model_id FROM sessions WHERE id = 'sess-1'`).Scan(&modelID))
	assert.Equal(t, "claude-fable-5", modelID, "existing column data survives the healing run")

	// And a second run of the v55 migration on the now-complete schema
	// converges: the pragma probe finds model_id, skips the ALTER, and
	// the version stays 55. (We re-run the specific step rather than the
	// full migrate() chain because this synthetic fixture is sessions-
	// only — later migrations touch other tables it deliberately omits.)
	require.NoError(t, migrateV54toV55(d), "re-run of v55 on the healed DB")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 55, ver, "re-run is a converging no-op, stays at 55")
}

// TestMigrateV54toV55_FreshSchemaRoundTrips builds a fresh DB through
// the full migrate() chain and round-trips model_id through the
// production helpers the statusline hook and inheritedLaunchFlags use.
// The literal currentVersion pin (the "next migration moves it forward"
// tripwire) now lives in the v56 test; here we only assert the fresh DB
// reaches whatever the current head is.
func TestMigrateV54toV55_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion pin moved forward to the v57 test
	// (TestMigrateV56toV57_FreshSchemaRoundTrips) — the next migration's
	// author carries it on, per the comment above.

	require.NoError(t, SaveSession(&SessionRow{ID: "sess-1", TmuxSession: "t1", Status: "running"}))

	// Empty write is a no-op (older Claude Code without model.id must
	// never blank a recorded value)…
	require.NoError(t, UpdateSessionModelID("sess-1", "claude-fable-5"))
	require.NoError(t, UpdateSessionModelID("sess-1", ""))

	snap, err := GetContextSnapshot("sess-1")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, "claude-fable-5", snap.ModelID, "model id round-trips; empty write didn't blank it")
}
