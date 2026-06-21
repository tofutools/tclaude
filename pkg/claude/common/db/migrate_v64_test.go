package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV63toV64_AddsAskThreads seeds a bare v63 DB, runs the v64
// migration, and asserts the ask_threads table exists, round-trips a row, and
// the version lands on 64.
func TestMigrateV63toV64_AddsAskThreads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v63.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (63);
	`)
	require.NoError(t, err, "seed v63 schema")

	require.NoError(t, migrateV63toV64(d), "migrateV63toV64")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 64, ver, "schema_version after migration")

	_, err = d.Exec(
		`INSERT INTO ask_threads (term_key, cwd, conv_id, harness, created_at, updated_at)
		 VALUES ('term-1', '/repo/x', 'conv-abc', 'claude', 'now', 'now')`)
	require.NoError(t, err, "insert into ask_threads")

	var conv string
	require.NoError(t, d.QueryRow(
		`SELECT conv_id FROM ask_threads WHERE term_key = 'term-1' AND cwd = '/repo/x'`,
	).Scan(&conv))
	assert.Equal(t, "conv-abc", conv, "row round-trips")
}

// TestMigrateV63toV64_HealsOnReRun guards the converge-on-re-run property:
// CREATE TABLE IF NOT EXISTS means a re-run (or a half-applied earlier run
// that already created the table) must not wedge on "table already exists" —
// the version still lands on 64 and a second run is a clean no-op.
func TestMigrateV63toV64_HealsOnReRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v63-rerun.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// ask_threads already present but version still 63 (a half-applied run).
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (63);
		CREATE TABLE ask_threads (
			term_key   TEXT NOT NULL,
			cwd        TEXT NOT NULL,
			conv_id    TEXT NOT NULL,
			harness    TEXT NOT NULL DEFAULT 'claude',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (term_key, cwd)
		);
	`)
	require.NoError(t, err, "seed half-applied v63 schema")

	require.NoError(t, migrateV63toV64(d), "re-run must converge, not fail on existing table")
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 64, ver, "schema_version finally lands on 64")

	require.NoError(t, migrateV63toV64(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 64, ver, "second re-run stays at 64")
}

// TestMigrateV63toV64_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts ask_threads exists. v64 is head, so this is where the
// literal currentVersion pin lives — the tripwire the next migration's author
// moves forward into their own v65 test.
func TestMigrateV63toV64_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v63 test —
	// the next migration's author moves it into their own v65 test.
	require.Equal(t, 64, currentVersion, "currentVersion is 64")

	var haveTable int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'ask_threads'`,
	).Scan(&haveTable))
	assert.Equal(t, 1, haveTable, "fresh schema has ask_threads")
}
