package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV83toV84_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. The literal currentVersion
// tripwire moved forward to the v85 test (head).
func TestMigrateV83toV84_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
}

// TestMigrateV83toV84_AddsColumn drives the real v83→v84 ALTER over a v83-pinned
// DB: it asserts agents.initial_spawn_config appears, that an existing agent row
// reads back as "" (no recorded config), that the version advances, and that a
// re-run is a clean no-op.
func TestMigrateV83toV84_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v83 and drop the new column so we re-add it from a true v83
	// shape (the fresh chain already ran v84). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE agents DROP COLUMN initial_spawn_config`)
	mustExec(t, d, `UPDATE schema_version SET version = 83`)

	// A pre-existing agent row (without the new column) must survive the ALTER
	// and read back with the default.
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_existing', 'conv-existing', '2026-06-29T00:00:00Z')`)

	require.NoError(t, migrateV83toV84(d), "v83→v84")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'initial_spawn_config'`).Scan(&n))
	assert.Equal(t, 1, n, "agents.initial_spawn_config added")

	var cfg string
	require.NoError(t, d.QueryRow(
		`SELECT initial_spawn_config FROM agents WHERE agent_id = 'agt_existing'`).Scan(&cfg))
	assert.Equal(t, "", cfg, "existing row defaults to no recorded config")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 84, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV83toV84(d), "v83→v84 re-run is a clean no-op")
}

// TestSetAgentInitialSpawnConfig_RoundTrip exercises the write helper: the
// verbatim JSON it stores reads back byte-for-byte off the row.
func TestSetAgentInitialSpawnConfig_RoundTrip(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("conv-cfg", "spawn")
	require.NoError(t, err)

	const cfg = `{"name":"worker","model":"opus[1m]","effort":"high"}`
	require.NoError(t, SetAgentInitialSpawnConfig(agentID, cfg))

	var got string
	require.NoError(t, d.QueryRow(
		`SELECT initial_spawn_config FROM agents WHERE agent_id = ?`, agentID).Scan(&got))
	assert.Equal(t, cfg, got, "stored verbatim")

	// Unknown agent is a no-op, not an error.
	require.NoError(t, SetAgentInitialSpawnConfig("agt_nope", cfg))
}
