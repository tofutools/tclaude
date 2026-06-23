package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV67toV68_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts export_jobs.worker_conv_id landed. v68 is head, so this is
// where the literal currentVersion pin now lives — the tripwire the next
// migration's author moves forward into their own v69 test.
func TestMigrateV67toV68_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v67 test.
	require.Equal(t, 68, currentVersion, "currentVersion is 68")

	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('export_jobs') WHERE name = 'worker_conv_id'`,
	).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "fresh schema has export_jobs.worker_conv_id")
}

// TestMigrateV67toV68_AddsWorkerConvID seeds a bare v67-shaped export_jobs table,
// runs the v68 migration, and asserts worker_conv_id lands as NOT NULL DEFAULT ''
// — a pre-existing row reads "" (not NULL), and the column accepts a clone conv-id.
func TestMigrateV67toV68_AddsWorkerConvID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v67.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (67);
		CREATE TABLE export_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conv_id TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO export_jobs (id, conv_id, status, created_at, updated_at)
			VALUES (1, 'orig-conv', 'requested', '2026-06-23T00:00:00Z', '2026-06-23T00:00:00Z');
	`)
	require.NoError(t, err, "seed v67 export_jobs")

	require.NoError(t, migrateV67toV68(d), "migrateV67toV68")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 68, ver, "schema_version after migration")

	// Pre-existing row reads the empty-string default, not NULL.
	var worker string
	require.NoError(t, d.QueryRow(`SELECT worker_conv_id FROM export_jobs WHERE id = 1`).Scan(&worker))
	assert.Equal(t, "", worker, "pre-v68 row reads worker_conv_id as ''")

	// The column accepts a clone conv-id write.
	_, err = d.Exec(`UPDATE export_jobs SET worker_conv_id = 'clone-conv' WHERE id = 1`)
	require.NoError(t, err, "worker_conv_id accepts a clone conv-id")
}

// TestMigrateV67toV68_HealsHalfAppliedRun guards the wedge class: an interrupted
// earlier attempt added the column but never bumped schema_version. The
// pragma_table_info probe makes the re-run skip the already-added column and land
// on 68 — and a second re-run is a no-op.
func TestMigrateV67toV68_HealsHalfAppliedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v67-half.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Half-applied: export_jobs already has worker_conv_id (with a value, to prove
	// the re-run preserves it); version still 67.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (67);
		CREATE TABLE export_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conv_id TEXT NOT NULL,
			worker_conv_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO export_jobs (id, conv_id, worker_conv_id, status, created_at, updated_at)
			VALUES (1, 'orig-conv', 'clone-conv', 'cloning', '2026-06-23T00:00:00Z', '2026-06-23T00:00:00Z');
	`)
	require.NoError(t, err, "seed half-applied v67 schema")

	require.NoError(t, migrateV67toV68(d), "re-run must converge, not fail on duplicate column")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 68, ver, "schema_version finally lands on 68")

	// The already-present value survived.
	var worker string
	require.NoError(t, d.QueryRow(`SELECT worker_conv_id FROM export_jobs WHERE id = 1`).Scan(&worker))
	assert.Equal(t, "clone-conv", worker, "existing worker_conv_id survives")

	// Second re-run: the probe finds the column, the ALTER skips, stays 68.
	require.NoError(t, migrateV67toV68(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 68, ver, "second re-run stays at 68")
}
