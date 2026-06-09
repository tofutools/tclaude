package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV48toV49_AddsPendingConvColumn seeds a bare v48 DB, runs
// the v49 migration, and asserts the sessions.pending_conv column lands
// with the right default and is writable. Plain ALTER TABLE ADD COLUMN
// migration — a pre-existing row reads back the '' default.
func TestMigrateV48toV49_AddsPendingConvColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v48.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v48 sessions table with one pre-existing row.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (48);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'idle');
		INSERT INTO sessions (id, status) VALUES ('pre-existing', 'idle');
	`)
	require.NoError(t, err, "seed v48 schema")

	require.NoError(t, migrateV48toV49(d), "migrateV48toV49")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 49, ver, "schema_version after migration")

	// The pre-existing row defaults to no announced transition.
	var pending string
	require.NoError(t, d.QueryRow(`SELECT pending_conv FROM sessions WHERE id = 'pre-existing'`).Scan(&pending))
	assert.Equal(t, "", pending, "pre-existing row defaults pending_conv to ''")

	// The column is writable.
	_, err = d.Exec(`UPDATE sessions SET pending_conv = 'abc-123' WHERE id = 'pre-existing'`)
	require.NoError(t, err, "write pending_conv")
	require.NoError(t, d.QueryRow(`SELECT pending_conv FROM sessions WHERE id = 'pre-existing'`).Scan(&pending))
	assert.Equal(t, "abc-123", pending, "pending_conv round-trips")
}

// TestMigrateV48toV49_FreshSchemaHasPendingConvColumn builds a fresh DB
// through the full migrate() chain and confirms sessions.pending_conv
// exists and the Set/GetSessionPendingConv accessors work end to end.
// Carries the literal currentVersion pin — a tripwire the next
// migration's author moves forward into their own v50 test.
func TestMigrateV48toV49_FreshSchemaHasPendingConvColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 49, currentVersion, "currentVersion is 49")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", Status: "idle"}), "SaveSession")
	require.NoError(t, SetSessionPendingConv("s1", "11111111-2222-3333-4444-555555555555"),
		"SetSessionPendingConv on a fresh schema")
	got, err := GetSessionPendingConv("s1")
	require.NoError(t, err, "GetSessionPendingConv")
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", got)

	// Unknown session reads back "" rather than erroring — the hook
	// callback's foreign-event guard treats that as "nothing announced".
	got, err = GetSessionPendingConv("nope")
	require.NoError(t, err, "GetSessionPendingConv on unknown id")
	assert.Equal(t, "", got)
}
