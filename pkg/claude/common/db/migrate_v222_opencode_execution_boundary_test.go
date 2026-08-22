package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV221toV222AddsOpenCodeExecutionBoundary(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v221.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (221)`)
	// Exact intermediate schema shipped by the original v221 change.
	mustExec(t, d, `CREATE TABLE opencode_runtimes (
		session_id TEXT PRIMARY KEY,
		sandbox_launch_spec_json TEXT NOT NULL DEFAULT ''
	) STRICT`)
	mustExec(t, d, `INSERT INTO opencode_runtimes
		(session_id, sandbox_launch_spec_json) VALUES ('runtime-one', '{"version":4}')`)

	require.NoError(t, migrateV221toV222(d))
	assert.Equal(t, 222, schemaVersion(d))
	var spec, boundary string
	require.NoError(t, d.QueryRow(`SELECT sandbox_launch_spec_json, execution_boundary_json
		FROM opencode_runtimes WHERE session_id = 'runtime-one'`).Scan(&spec, &boundary))
	assert.JSONEq(t, `{"version":4}`, spec)
	assert.Empty(t, boundary)
	require.NoError(t, migrateV221toV222(d), "partially applied migration converges")
}
