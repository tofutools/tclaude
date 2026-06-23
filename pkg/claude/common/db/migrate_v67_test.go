package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV66toV67_AddsAuditLog seeds a bare v66 schema, runs the v67
// migration, and asserts the audit_log table + its `at` index land and
// accept a row. The migration is a CREATE TABLE IF NOT EXISTS, so it is
// idempotent by construction — a second run is a no-op that stays on 67.
func TestMigrateV66toV67_AddsAuditLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v66.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (66);
	`)
	require.NoError(t, err, "seed v66 schema")

	require.NoError(t, migrateV66toV67(d), "migrateV66toV67")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 67, ver, "schema_version after migration")

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

	// Second run is a no-op and stays on 67.
	require.NoError(t, migrateV66toV67(d), "second re-run is a no-op")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 67, ver, "second re-run stays at 67")
}

// TestMigrateV66toV67_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts audit_log exists. v67 is head, so this is
// where the literal currentVersion tripwire now lives — the next
// migration's author moves it forward into their own v68 test.
func TestMigrateV66toV67_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// The literal currentVersion tripwire, moved forward from the v66 test —
	// the next migration's author moves it into their own v68 test.
	require.Equal(t, 67, currentVersion, "currentVersion is 67")

	var haveTable int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'audit_log'`).Scan(&haveTable))
	assert.Equal(t, 1, haveTable, "fresh schema has audit_log")
}
