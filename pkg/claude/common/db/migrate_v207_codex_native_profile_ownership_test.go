package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV206toV207AddsAndBackfillsNativeProfileOwnership(t *testing.T) {
	assert.Equal(t, 208, currentVersion, "tripwire: bump this with the next migration")
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v206.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (206)`)
	mustExec(t, d, `CREATE TABLE codex_app_server_runtimes (
		generation TEXT PRIMARY KEY, launch_id TEXT NOT NULL, agent_id TEXT NOT NULL,
		conv_id TEXT NOT NULL, state TEXT NOT NULL) STRICT`)
	mustExec(t, d, `CREATE TABLE codex_native_permission_profiles (
		generation TEXT PRIMARY KEY, profile_name TEXT NOT NULL UNIQUE,
		profile_toml TEXT NOT NULL, cleanup_pending INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL) STRICT`)
	mustExec(t, d, `INSERT INTO codex_app_server_runtimes VALUES
		('generation', 'launch', 'agent', 'conv', 'ready')`)
	mustExec(t, d, `INSERT INTO codex_native_permission_profiles VALUES
		('generation', 'tclaude-agent-1234567890abcdef', 'complete', 0, 1)`)

	require.NoError(t, migrateV206toV207(d))
	assert.Equal(t, 207, schemaVersion(d))
	var agent, conv, launch string
	var ready bool
	require.NoError(t, d.QueryRow(`SELECT owner_agent_id, owner_conv_id, launch_id, launch_ready
		FROM codex_native_permission_profiles WHERE generation = 'generation'`).
		Scan(&agent, &conv, &launch, &ready))
	assert.Equal(t, "agent", agent)
	assert.Equal(t, "conv", conv)
	assert.Equal(t, "launch", launch)
	assert.True(t, ready)
	require.NoError(t, migrateV206toV207(d), "partially applied migration converges")
}
