package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV134toV135AddsProcessSnippetsAdditively(t *testing.T) {
	require.Equal(t, 136, currentVersion, "tripwire: bump this with the next migration")
	d, err := sql.Open("sqlite", "file:migrate-v135?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (134)`)
	mustExec(t, d, `CREATE TABLE downgrade_sentinel (value TEXT NOT NULL)`)
	mustExec(t, d, `INSERT INTO downgrade_sentinel VALUES ('preserve')`)

	require.NoError(t, migrateV134toV135(d))
	assert.Equal(t, 135, schemaVersion(d))
	require.NoError(t, migrateV134toV135(d), "partial/repeated migration converges")

	for _, table := range []string{"process_snippet_library", "process_snippets", "downgrade_sentinel"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
	var sentinel string
	require.NoError(t, d.QueryRow(`SELECT value FROM downgrade_sentinel`).Scan(&sentinel))
	assert.Equal(t, "preserve", sentinel, "additive migration leaves older schema data untouched")
	var generation int
	require.NoError(t, d.QueryRow(`SELECT generation FROM process_snippet_library WHERE id = 1`).Scan(&generation))
	assert.Zero(t, generation)
}

func TestFreshSchemaHasProcessSnippets(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	for _, table := range []string{"process_snippet_library", "process_snippets"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
}
