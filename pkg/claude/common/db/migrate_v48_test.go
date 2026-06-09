package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV47toV48_AddsEffortColumn seeds a bare v47 DB, runs the v48
// migration, and asserts the sessions.effort_level column lands with the
// right default and is writable. Plain ALTER TABLE ADD COLUMN migration —
// a pre-existing row reads back the '' default.
func TestMigrateV47toV48_AddsEffortColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v47.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v47 sessions table with one pre-existing row.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (47);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'idle');
		INSERT INTO sessions (id, status) VALUES ('pre-existing', 'idle');
	`)
	require.NoError(t, err, "seed v47 schema")

	require.NoError(t, migrateV47toV48(d), "migrateV47toV48")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 48, ver, "schema_version after migration")

	// The pre-existing row defaults to an empty effort level.
	var effort string
	require.NoError(t, d.QueryRow(`SELECT effort_level FROM sessions WHERE id = 'pre-existing'`).Scan(&effort))
	assert.Equal(t, "", effort, "pre-existing row defaults effort_level to ''")

	// The column is writable.
	_, err = d.Exec(`UPDATE sessions SET effort_level = 'high' WHERE id = 'pre-existing'`)
	require.NoError(t, err, "write effort_level")
	require.NoError(t, d.QueryRow(`SELECT effort_level FROM sessions WHERE id = 'pre-existing'`).Scan(&effort))
	assert.Equal(t, "high", effort, "effort_level round-trips")
}

// TestMigrateV47toV48_FreshSchemaHasEffortColumn builds a fresh DB through
// the full migrate() chain and confirms sessions.effort_level exists and
// the UpdateSessionEffort / GetContextSnapshot accessors work end to end.
// (The literal currentVersion tripwire pin lives in the newest
// migration's test — see migrate_v49_test.go.)
func TestMigrateV47toV48_FreshSchemaHasEffortColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", Status: "idle"}), "SaveSession")
	require.NoError(t, UpdateSessionEffort("s1", "high"), "UpdateSessionEffort on a fresh schema")
	got, err := GetContextSnapshot("s1")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, "high", got.EffortLevel)
}

// TestUpdateSessionEffort_EmptyIsNoop confirms an empty level never blanks
// a recorded one — mirroring UpdateSessionModel's empty-string guard, so a
// stray statusline render without effort (pre-first-response, or a model
// without reasoning-effort support) leaves the last good value intact.
func TestUpdateSessionEffort_EmptyIsNoop(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{ID: "s1", Status: "idle"}), "SaveSession")

	require.NoError(t, UpdateSessionEffort("s1", "xhigh"), "set effort")
	require.NoError(t, UpdateSessionEffort("s1", ""), "empty effort is a no-op")

	got, err := GetContextSnapshot("s1")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, "xhigh", got.EffortLevel, "empty render must not blank a good value")
}
