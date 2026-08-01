package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV161toV162AddsOpenCodeRuntimeSandboxAuthority(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `ALTER TABLE opencode_runtimes DROP COLUMN sandbox_launch_spec_json`)
	mustExec(t, d, `ALTER TABLE opencode_runtimes DROP COLUMN sandbox_implementation`)
	mustExec(t, d, `UPDATE schema_version SET version = 161`)
	mustExec(t, d, `INSERT INTO opencode_runtimes
		(session_id, server_url, password, cwd, created_at, updated_at)
		VALUES ('spwn-legacy', 'http://127.0.0.1:43210', 'private', '/tmp/project', 'now', 'now')`)

	require.NoError(t, migrateV161toV162(d))
	assert.Equal(t, 162, schemaVersion(d))
	var implementation, spec string
	require.NoError(t, d.QueryRow(`SELECT sandbox_implementation, sandbox_launch_spec_json
		FROM opencode_runtimes WHERE session_id = 'spwn-legacy'`).Scan(&implementation, &spec))
	assert.Equal(t, "harness-builtin", implementation)
	assert.Empty(t, spec)
	require.NoError(t, migrateV161toV162(d), "partially applied migration converges")
	assert.Equal(t, 179, currentVersion, "tripwire: bump this with the next migration")
}
