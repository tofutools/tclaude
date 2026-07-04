package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV95toV96_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v96 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v95).
func TestMigrateV95toV96_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 96, currentVersion, "tripwire: bump this and add a v96→v97 test when you add a migration")
}

// TestMigrateV95toV96_CreatesAgentTags drives the real v95→v96 migration over a
// v95-pinned DB: it asserts the agent_tags table appears, that a re-run is a
// clean no-op (CREATE TABLE IF NOT EXISTS), and that the version advances.
func TestMigrateV95toV96_CreatesAgentTags(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v95 and drop the table so we re-create it from a true v95 shape
	// (the fresh chain already ran v96).
	mustExec(t, d, `DROP TABLE IF EXISTS agent_tags`)
	mustExec(t, d, `UPDATE schema_version SET version = 95`)

	require.NoError(t, migrateV95toV96(d), "v95→v96")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_tags'`).Scan(&n))
	assert.Equal(t, 1, n, "agent_tags table created")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 96, ver, "version advances to 96")

	// Re-run is a clean no-op (idempotent).
	require.NoError(t, migrateV95toV96(d), "v95→v96 re-run")
}

// TestAgentTags_CascadeOnAgentDelete proves the ON DELETE CASCADE foreign key:
// deleting an agent row drops its tags automatically (foreign_keys is enforced
// on every connection), so a tag can never outlive its actor.
func TestAgentTags_CascadeOnAgentDelete(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_tagged', 'conv-tagged', '2026-07-04T00:00:00Z')`)
	require.NoError(t, AddAgentTags("agt_tagged", "tf:squad", "priority"))

	tags, err := ListAgentTags("agt_tagged")
	require.NoError(t, err)
	assert.Equal(t, []string{"priority", "tf:squad"}, tags, "tags stored, sorted")

	mustExec(t, d, `DELETE FROM agents WHERE agent_id = 'agt_tagged'`)

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_tags WHERE agent_id = 'agt_tagged'`).Scan(&n))
	assert.Equal(t, 0, n, "cascade drops the actor's tags")
}
