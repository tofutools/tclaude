package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV196toV197AddsAgentLineageWithoutGuessingBackfill(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE agent_lineage`)
	mustExec(t, d, `UPDATE schema_version SET version = 196`)

	// reply_to is not spawn provenance: it can be a handoff target. Even a
	// syntactically resolvable value must not fabricate an authorization edge.
	mustExec(t, d, `INSERT INTO agents
		(agent_id, current_conv_id, created_at, created_via, initial_spawn_config)
		VALUES
		('agt_parent', 'parent-conv', 100, 'spawn', ''),
		('agt_child', 'child-conv', 200, 'spawn', '{"reply_to":"parent-conv"}')`)
	mustExec(t, d, `INSERT INTO agent_conversations
		(conv_id, agent_id, role, reason, linked_at) VALUES
		('parent-conv', 'agt_parent', 'head', 'spawn', 100),
		('child-conv', 'agt_child', 'head', 'spawn', 200)`)

	require.NoError(t, migrateV196toV197(d))
	assert.Equal(t, 197, schemaVersion(d))
	var rows int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_lineage`).Scan(&rows))
	assert.Zero(t, rows, "migration must not turn reply_to into retire authority")
	var strict int
	require.NoError(t, d.QueryRow(`SELECT strict FROM pragma_table_list
		WHERE name = 'agent_lineage'`).Scan(&strict))
	assert.Equal(t, 1, strict)
	require.NoError(t, migrateV196toV197(d), "partially applied migration converges")
}

func TestMigrateV196toV197SurvivesMinimalHealSchema(t *testing.T) {
	d, err := sql.Open("sqlite", t.TempDir()+"/minimal.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (196)`)

	require.NoError(t, migrateV196toV197(d))
	assert.Equal(t, 197, schemaVersion(d))
}
