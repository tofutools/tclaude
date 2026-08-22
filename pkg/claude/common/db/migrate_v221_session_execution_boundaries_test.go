package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV220toV221AddsSessionExecutionBoundaries(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v220.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (220)`)
	mustExec(t, d, `CREATE TABLE sessions (id TEXT PRIMARY KEY) STRICT`)
	mustExec(t, d, `INSERT INTO sessions VALUES ('spwn-one')`)
	mustExec(t, d, `CREATE TABLE opencode_runtimes (session_id TEXT PRIMARY KEY) STRICT`)

	require.NoError(t, migrateV220toV221(d))
	assert.Equal(t, 221, schemaVersion(d))
	mustExec(t, d, `INSERT INTO session_execution_boundaries VALUES ('spwn-one', '{"version":1}')`)
	var raw string
	require.NoError(t, d.QueryRow(`SELECT boundary_json FROM session_execution_boundaries WHERE session_id = 'spwn-one'`).Scan(&raw))
	assert.JSONEq(t, `{"version":1}`, raw)
	mustExec(t, d, `INSERT INTO opencode_runtimes (session_id) VALUES ('runtime-one')`)
	var openCodeBoundary string
	require.NoError(t, d.QueryRow(`SELECT execution_boundary_json FROM opencode_runtimes LIMIT 1`).Scan(&openCodeBoundary))
	assert.Empty(t, openCodeBoundary)
	require.NoError(t, migrateV220toV221(d), "partially applied migration converges")
}
