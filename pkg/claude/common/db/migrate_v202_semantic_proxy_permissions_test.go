package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV202RenamesEveryStoredSemanticProxyPermission(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v201.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (201);
		CREATE TABLE agents (agent_id TEXT PRIMARY KEY);
		CREATE TABLE agent_groups (id INTEGER PRIMARY KEY, owner_scopes_json TEXT);
		CREATE TABLE agent_permissions (agent_id TEXT, slug TEXT, effect TEXT, PRIMARY KEY(agent_id, slug));
		CREATE TABLE agent_group_permissions (group_id INTEGER, slug TEXT, PRIMARY KEY(group_id, slug));
		CREATE TABLE agent_sudo_grants (id INTEGER PRIMARY KEY, slug TEXT);
		CREATE TABLE access_requests (id TEXT PRIMARY KEY, perm TEXT);
		CREATE TABLE roles (id INTEGER PRIMARY KEY, permissions TEXT);
		CREATE TABLE group_template_agents (id INTEGER PRIMARY KEY, permissions TEXT, profile_inline TEXT);
		CREATE TABLE pending_spawns (id INTEGER PRIMARY KEY, permission_overrides TEXT);
		CREATE TABLE spawn_profiles (id INTEGER PRIMARY KEY, permission_overrides TEXT);
		CREATE TABLE group_templates (id INTEGER PRIMARY KEY, owner_scopes_json TEXT);
		INSERT INTO agents VALUES ('a');
		INSERT INTO agent_permissions VALUES ('a', 'git.read', 'grant');
		INSERT INTO agent_permissions VALUES ('a', 'proxy.git.read', 'deny');
		INSERT INTO agent_group_permissions VALUES (1, 'github.write');
		INSERT INTO agent_sudo_grants VALUES (1, 'linear.read');
		INSERT INTO access_requests VALUES ('r', 'git.push');
		INSERT INTO roles VALUES (1, '["github.read",{"slug":"linear.write","scope":{"linear_team":["TCL"]}}]');
		INSERT INTO group_template_agents VALUES (1,
			'["git.read","proxy.git.read"]',
			'{"model":"opus","permission_overrides":{"git.push":"grant"}}');
		INSERT INTO pending_spawns VALUES (1, '{"linear.read":"grant"}');
		INSERT INTO spawn_profiles VALUES (1, '{"github.write":"deny"}');
		INSERT INTO agent_groups VALUES (1, '{"git.read":{"git_remote":["example.com/org/repo"]}}');
		INSERT INTO group_templates VALUES (1, '{"linear.write":{"linear_team":["TCL"]}}');
	`)

	require.NoError(t, migrateV201toV202(d))
	assert.Equal(t, 202, schemaVersion(d))
	require.NoError(t, migrateV201toV202(d), "partially applied migration converges")

	assertRowValue(t, d, `SELECT slug FROM agent_permissions WHERE agent_id = 'a'`, "proxy.git.read")
	assertRowValue(t, d, `SELECT effect FROM agent_permissions WHERE agent_id = 'a'`, "deny")
	assertRowValue(t, d, `SELECT slug FROM agent_group_permissions`, "proxy.github.write")
	assertRowValue(t, d, `SELECT slug FROM agent_sudo_grants`, "proxy.linear.read")
	assertRowValue(t, d, `SELECT perm FROM access_requests`, "proxy.git.push")
	assertRowValue(t, d, `SELECT permissions FROM roles`,
		`["proxy.github.read",{"slug":"proxy.linear.write","scope":{"linear_team":["TCL"]}}]`)
	assertRowValue(t, d, `SELECT permissions FROM group_template_agents`, `["proxy.git.read"]`)
	assertRowValue(t, d, `SELECT profile_inline FROM group_template_agents`,
		`{"model":"opus","permission_overrides":{"proxy.git.push":"grant"}}`)
	assertRowValue(t, d, `SELECT permission_overrides FROM pending_spawns`, `{"proxy.linear.read":"grant"}`)
	assertRowValue(t, d, `SELECT permission_overrides FROM spawn_profiles`, `{"proxy.github.write":"deny"}`)
	assertRowValue(t, d, `SELECT owner_scopes_json FROM agent_groups`,
		`{"proxy.git.read":{"git_remote":["example.com/org/repo"]}}`)
	assertRowValue(t, d, `SELECT owner_scopes_json FROM group_templates`,
		`{"proxy.linear.write":{"linear_team":["TCL"]}}`)
}

func TestMigrateV202ToleratesOrphanedHealTables(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v201-partial.sqlite")+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (201);
		CREATE TABLE agent_group_permissions (
			group_id INTEGER REFERENCES agent_groups(id),
			slug TEXT,
			PRIMARY KEY(group_id, slug)
		);
	`)
	require.NoError(t, migrateV201toV202(d))
	assert.Equal(t, 202, schemaVersion(d))
}

func assertRowValue(t *testing.T, d *sql.DB, query, want string) {
	t.Helper()
	var got string
	require.NoError(t, d.QueryRow(query).Scan(&got))
	assert.Equal(t, want, got)
}
