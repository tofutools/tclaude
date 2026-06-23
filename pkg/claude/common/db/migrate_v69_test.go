package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV68toV69_AddsAuditLog seeds a bare v68 schema, runs the v69
// migration, and asserts the audit_log table + its `at` index land and
// accept a row. The migration is a CREATE TABLE IF NOT EXISTS, so it is
// idempotent by construction — a second run is a no-op that stays on 69.
func TestMigrateV68toV69_AddsAuditLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v68.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (68);
	`)
	require.NoError(t, err, "seed v68 schema")

	require.NoError(t, migrateV68toV69(d), "migrateV68toV69")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 69, ver, "schema_version after migration")

	// The table exists and accepts an insert.
	_, err = d.Exec(`
		INSERT INTO audit_log (at, actor_kind, verb, status, source)
		VALUES ('2026-06-23T00:00:00Z', 'human', 'spawn', 200, 'cli')`)
	require.NoError(t, err, "audit_log accepts a row")

	// The at index exists (it backs the retention prune).
	var idx int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_audit_log_at'`).Scan(&idx))
	assert.Equal(t, 1, idx, "idx_audit_log_at exists")

	// Second run is a no-op and stays on 69.
	require.NoError(t, migrateV68toV69(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 69, ver, "second re-run stays at 69")
}

// TestMigrateV68toV69_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts audit_log exists. v69 is head, so this is
// where the literal currentVersion tripwire now lives — the next
// migration's author moves it forward into their own v70 test.
func TestMigrateV68toV69_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v68 test —
	// the next migration's author moves it into their own v70 test.
	require.Equal(t, 69, currentVersion, "currentVersion is 69")

	var haveTable int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'audit_log'`).Scan(&haveTable))
	assert.Equal(t, 1, haveTable, "fresh schema has audit_log")
}
