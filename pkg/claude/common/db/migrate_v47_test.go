package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV46toV47_AddsModelColumn seeds a bare v46 DB, runs the v47
// migration, and asserts the sessions.model column lands with the right
// default and is writable. Plain ALTER TABLE ADD COLUMN migration — a
// pre-existing row reads back the '' default.
func TestMigrateV46toV47_AddsModelColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v46.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v46 sessions table with one pre-existing row.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (46);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'idle');
		INSERT INTO sessions (id, status) VALUES ('pre-existing', 'idle');
	`)
	require.NoError(t, err, "seed v46 schema")

	require.NoError(t, migrateV46toV47(d), "migrateV46toV47")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 47, ver, "schema_version after migration")

	// The pre-existing row defaults to an empty model.
	var model string
	require.NoError(t, d.QueryRow(`SELECT model FROM sessions WHERE id = 'pre-existing'`).Scan(&model))
	assert.Equal(t, "", model, "pre-existing row defaults model to ''")

	// The column is writable.
	_, err = d.Exec(`UPDATE sessions SET model = 'Opus 4.8' WHERE id = 'pre-existing'`)
	require.NoError(t, err, "write model")
	require.NoError(t, d.QueryRow(`SELECT model FROM sessions WHERE id = 'pre-existing'`).Scan(&model))
	assert.Equal(t, "Opus 4.8", model, "model round-trips")
}

// TestMigrateV46toV47_FreshSchemaHasModelColumn builds a fresh DB through
// the full migrate() chain and confirms sessions.model exists and the
// UpdateSessionModel / GetContextSnapshot accessors work end to end.
// The literal currentVersion pin moved forward to migrate_v48_test.go's
// TestMigrateV47toV48_FreshSchemaHasEffortColumn — the next migration's
// author moves it again.
func TestMigrateV46toV47_FreshSchemaHasModelColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", Status: "idle"}), "SaveSession")
	require.NoError(t, UpdateSessionModel("s1", "Opus 4.8"), "UpdateSessionModel on a fresh schema")
	got, err := GetContextSnapshot("s1")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, "Opus 4.8", got.Model)
}
