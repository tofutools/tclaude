package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV59toV60_DropsCompactPending seeds a bare v59 DB whose
// sessions table still carries the removed auto-compact bookkeeping
// column, runs the v60 migration, and asserts the column is gone and the
// version lands on 60.
func TestMigrateV59toV60_DropsCompactPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v59.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (59);
		CREATE TABLE sessions (
			id              TEXT PRIMARY KEY,
			context_pct     REAL NOT NULL DEFAULT 0,
			compact_pending REAL NOT NULL DEFAULT 0,
			nudged_pct      REAL NOT NULL DEFAULT 0
		);
		INSERT INTO sessions (id, context_pct, compact_pending, nudged_pct)
			VALUES ('sess-a', 42.0, 1700000000, 30);
	`)
	require.NoError(t, err, "seed v59 schema")

	require.NoError(t, migrateV59toV60(d), "migrateV59toV60")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 60, ver, "schema_version after migration")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'compact_pending'`,
	).Scan(&haveCol))
	assert.Equal(t, 0, haveCol, "compact_pending column is dropped")

	// The surviving out-of-band columns keep their values.
	var pct, nudged float64
	require.NoError(t, d.QueryRow(`SELECT context_pct, nudged_pct FROM sessions WHERE id = 'sess-a'`).Scan(&pct, &nudged))
	assert.Equal(t, 42.0, pct, "context_pct survives the column drop")
	assert.Equal(t, 30.0, nudged, "nudged_pct survives the column drop")
}

// TestMigrateV59toV60_HealsMissingColumn guards the converge-on-re-run
// property: a DB whose sessions table never had compact_pending (or a
// re-run after a prior successful drop) must not wedge on "no such
// column" — the pragma_table_info probe skips the DROP and the version
// still lands on 60. A second re-run is a clean no-op.
func TestMigrateV59toV60_HealsMissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v59-nocol.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// sessions exists but already lacks compact_pending.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (59);
		CREATE TABLE sessions (
			id          TEXT PRIMARY KEY,
			context_pct REAL NOT NULL DEFAULT 0,
			nudged_pct  REAL NOT NULL DEFAULT 0
		);
	`)
	require.NoError(t, err, "seed v59 schema without compact_pending")

	require.NoError(t, migrateV59toV60(d), "re-run must converge, not fail on missing column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 60, ver, "schema_version finally lands on 60")

	require.NoError(t, migrateV59toV60(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 60, ver, "second re-run stays at 60")
}

// TestMigrateV59toV60_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts the sessions table has no compact_pending
// column. Carries the literal currentVersion pin — the tripwire the next
// migration's author moves forward into their own v61 test.
func TestMigrateV59toV60_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 60, currentVersion, "currentVersion is 60")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'compact_pending'`,
	).Scan(&haveCol))
	assert.Equal(t, 0, haveCol, "fresh schema has no compact_pending column")
}
