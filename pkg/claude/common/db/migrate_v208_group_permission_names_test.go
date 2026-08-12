package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV208RenamesEveryStoredGroupPermission(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v207.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (207);
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
		INSERT INTO agent_permissions VALUES ('a', 'groups.spawn', 'grant');
		INSERT INTO agent_permissions VALUES ('a', 'groups.members.spawn', 'deny');
		INSERT INTO agent_group_permissions VALUES (1, 'member.add');
		INSERT INTO access_requests VALUES ('r', 'groups.default-dir');
		INSERT INTO roles VALUES (1, '["groups.stop",{"slug":"groups.own","scope":{"group":["alpha"]}}]');
		INSERT INTO group_template_agents VALUES (1,
			'["member.remove","groups.members.remove"]',
			'{"model":"opus","permission_overrides":{"groups.notifications":"grant"}}');
		INSERT INTO pending_spawns VALUES (1, '{"groups.remote-control":"grant"}');
		INSERT INTO spawn_profiles VALUES (1, '{"member.redesignate":"deny"}');
		INSERT INTO agent_groups VALUES (1, '{"groups.spawn":{"spawn_profile":["reviewer"]}}');
		INSERT INTO group_templates VALUES (1, '{"groups.owner-scopes":{"group":["alpha"]}}');
	`)
	for legacy := range groupPermissionRenames {
		_, err := d.Exec(`INSERT INTO agent_sudo_grants (slug) VALUES (?)`, legacy)
		require.NoError(t, err)
	}

	require.NoError(t, migrateV207toV208(d))
	assert.Equal(t, 208, schemaVersion(d))
	require.NoError(t, migrateV207toV208(d), "partially applied migration converges")

	assertRowValue(t, d, `SELECT slug FROM agent_permissions WHERE agent_id = 'a'`, "groups.members.spawn")
	assertRowValue(t, d, `SELECT effect FROM agent_permissions WHERE agent_id = 'a'`, "deny")
	assertRowValue(t, d, `SELECT slug FROM agent_group_permissions`, "groups.members.add")
	for legacy, canonical := range groupPermissionRenames {
		var legacyCount, canonicalCount int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_sudo_grants WHERE slug = ?`, legacy).Scan(&legacyCount))
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_sudo_grants WHERE slug = ?`, canonical).Scan(&canonicalCount))
		assert.Zero(t, legacyCount, legacy)
		assert.Equal(t, 1, canonicalCount, canonical)
	}
	assertRowValue(t, d, `SELECT perm FROM access_requests`, "groups.settings.default-dir")
	assertRowValue(t, d, `SELECT permissions FROM roles`,
		`["groups.members.stop",{"slug":"groups.owners.manage","scope":{"group":["alpha"]}}]`)
	assertRowValue(t, d, `SELECT permissions FROM group_template_agents`, `["groups.members.remove"]`)
	assertRowValue(t, d, `SELECT profile_inline FROM group_template_agents`,
		`{"model":"opus","permission_overrides":{"groups.settings.notifications":"grant"}}`)
	assertRowValue(t, d, `SELECT permission_overrides FROM pending_spawns`,
		`{"groups.settings.remote-control-policy":"grant"}`)
	assertRowValue(t, d, `SELECT permission_overrides FROM spawn_profiles`,
		`{"groups.members.update":"deny"}`)
	assertRowValue(t, d, `SELECT owner_scopes_json FROM agent_groups`,
		`{"groups.members.spawn":{"spawn_profile":["reviewer"]}}`)
	assertRowValue(t, d, `SELECT owner_scopes_json FROM group_templates`,
		`{"groups.settings.owner-scopes":{"group":["alpha"]}}`)
}

func TestCanonicalPermissionSlugIncludesHistoricalRenames(t *testing.T) {
	assert.Equal(t, "groups.members.spawn", CanonicalPermissionSlug("groups.spawn"))
	assert.Equal(t, "proxy.github.read", CanonicalPermissionSlug("github.read"))
	assert.Equal(t, "self.rename", CanonicalPermissionSlug("self.rename"))
}
