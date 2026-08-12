package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV210RenamesOnlyOwnerConstraintKeys(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v209.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (209);
		CREATE TABLE agent_groups (id INTEGER PRIMARY KEY, owner_scopes_json TEXT);
		CREATE TABLE group_templates (id INTEGER PRIMARY KEY, owner_scopes_json TEXT);
		CREATE TABLE agent_permissions (agent_id TEXT, slug TEXT);
		INSERT INTO agent_groups VALUES (1,
			'{"agent.retire":{"target_agent":["@descendants"]},"groups.members.retire":{"group":["canonical"]}}');
		INSERT INTO group_templates VALUES (1,
			'{"agent.clone":{"group":["alpha"]}}');
		INSERT INTO agent_permissions VALUES ('a', 'agent.clone');
	`)

	require.NoError(t, migrateV209toV210(d))
	assert.Equal(t, 210, schemaVersion(d))
	require.NoError(t, migrateV209toV210(d), "partially applied migration converges")

	// An already-canonical key wins a collision, matching the established
	// permission-map migration rule.
	assertRowValue(t, d, `SELECT owner_scopes_json FROM agent_groups`,
		`{"groups.members.retire":{"group":["canonical"]}}`)
	assertRowValue(t, d, `SELECT owner_scopes_json FROM group_templates`,
		`{"groups.members.clone":{"group":["alpha"]}}`)
	assertRowValue(t, d, `SELECT slug FROM agent_permissions`, "agent.clone")
}
