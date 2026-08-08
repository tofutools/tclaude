package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV194toV195AddsPermissionScopes(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	for _, table := range []string{"agent_permissions", "agent_group_permissions", "agent_sudo_grants"} {
		mustExec(t, d, `ALTER TABLE `+table+` DROP COLUMN scope_json`)
	}
	mustExec(t, d, `UPDATE schema_version SET version = 194`)

	require.NoError(t, migrateV194toV195(d))
	assert.Equal(t, 195, schemaVersion(d))
	for _, table := range []string{"agent_permissions", "agent_group_permissions", "agent_sudo_grants"} {
		var have int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'scope_json'`, table).Scan(&have))
		assert.Equal(t, 1, have, table)
	}

	// The storage boundary independently rejects a value larger than the wire
	// parser accepts, even for a writer that bypasses agentd validation.
	mustExec(t, d, `INSERT INTO agent_groups (name, created_at) VALUES ('scope-check', 1)`)
	_, err = d.Exec(`INSERT INTO agent_group_permissions
		(group_id, slug, granted_at, granted_by, scope_json)
		VALUES ((SELECT id FROM agent_groups WHERE name = 'scope-check'), 'groups.spawn', 1, 'test', ?)`,
		strings.Repeat("x", 262145))
	require.Error(t, err)
	require.NoError(t, migrateV194toV195(d), "partially applied migration converges")
}

func TestMigrateV194toV195SurvivesMinimalHealSchema(t *testing.T) {
	d, err := sql.Open("sqlite", t.TempDir()+"/minimal.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (194)`)

	require.NoError(t, migrateV194toV195(d))
	assert.Equal(t, 195, schemaVersion(d))
}
