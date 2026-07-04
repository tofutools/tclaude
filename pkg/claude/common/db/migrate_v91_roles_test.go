package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV90toV91_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. (The literal currentVersion
// tripwire moved forward to the v92 test — the newest head.)
func TestMigrateV90toV91_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
}

// TestMigrateV90toV91_AddsRolesAndRoleRef drives the real v90→v91 migration
// over a v90-pinned DB: it asserts the roles table appears, the role_ref
// column is added to group_template_agents, a pre-existing template agent
// reads role_ref back as '', the version advances, and a re-run is a clean
// no-op.
func TestMigrateV90toV91_AddsRolesAndRoleRef(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v90: drop the roles table and the role_ref column so we
	// re-create them from a true v90 shape (the fresh chain already ran v91).
	mustExec(t, d, `DROP TABLE IF EXISTS roles`)
	mustExec(t, d, `ALTER TABLE group_template_agents DROP COLUMN role_ref`)
	mustExec(t, d, `UPDATE schema_version SET version = 90`)

	// A pre-existing template + agent (without role_ref) must survive the ALTER.
	mustExec(t, d, `INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', '', '2026-07-03T00:00:00Z', '2026-07-03T00:00:00Z')`)
	var tid int64
	require.NoError(t, d.QueryRow(`SELECT id FROM group_templates WHERE name = 'legacy'`).Scan(&tid))
	mustExec(t, d, `INSERT INTO group_template_agents (template_id, ordinal, name)
		VALUES (?, 0, 'dev1')`, tid)

	require.NoError(t, migrateV90toV91(d), "v90→v91")

	// roles table exists.
	var haveRoles int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'roles'`).Scan(&haveRoles))
	assert.Equal(t, 1, haveRoles, "roles table created")

	// role_ref column exists on group_template_agents.
	var haveCol int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = 'role_ref'`).Scan(&haveCol))
	assert.Equal(t, 1, haveCol, "group_template_agents.role_ref added")

	// The legacy agent reads role_ref back as unset.
	tmpl, err := GetGroupTemplate("legacy")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 1)
	assert.Empty(t, tmpl.Agents[0].RoleRef, "legacy agent has no role_ref")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 91, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op.
	require.NoError(t, migrateV90toV91(d), "v90→v91 re-run is a clean no-op")
}

// TestRole_RoundTrip proves the CRUD helpers persist and read back a role.
func TestRole_RoundTrip(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	id, err := CreateRole(&Role{
		Name: "auditor", Descr: "d", Brief: "You audit.",
		Harness: "claude", Model: "opus", Permissions: []string{"human.notify"},
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	rl, err := GetRole("auditor")
	require.NoError(t, err)
	require.NotNil(t, rl)
	assert.Equal(t, "auditor", rl.Name)
	assert.Equal(t, "You audit.", rl.Brief)
	assert.Equal(t, "claude", rl.Harness)
	assert.Equal(t, "opus", rl.Model)
	assert.Equal(t, []string{"human.notify"}, rl.Permissions)

	// A duplicate name surfaces as ErrRoleNameTaken.
	_, err = CreateRole(&Role{Name: "auditor"})
	assert.ErrorIs(t, err, ErrRoleNameTaken)

	// Update in place.
	rl.Descr = "d2"
	rl.Brief = "You audit carefully."
	require.NoError(t, UpdateRole(rl))
	got, err := GetRole("auditor")
	require.NoError(t, err)
	assert.Equal(t, "d2", got.Descr)
	assert.Equal(t, "You audit carefully.", got.Brief)

	// Delete.
	n, err := DeleteRole("auditor")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	gone, err := GetRole("auditor")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

// TestEnsureSeededRoles proves the canonical roles are seeded on Open and that
// a user's edit to a seed survives a re-seed (edits are sacred), while a
// deleted seed is re-added.
func TestEnsureSeededRoles(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Every canonical seed is present after Open.
	for _, s := range seedRoles {
		rl, err := GetRole(s.name)
		require.NoErrorf(t, err, "GetRole(%q)", s.name)
		require.NotNilf(t, rl, "seed role %q present", s.name)
		assert.NotEmptyf(t, rl.Brief, "seed role %q has a brief", s.name)
	}

	// Edit a seed, then re-seed: the edit must survive (never overwritten).
	dev, err := GetRole("dev")
	require.NoError(t, err)
	dev.Brief = "CUSTOM BRIEF"
	require.NoError(t, UpdateRole(dev))
	require.NoError(t, ensureSeededRoles(d))
	after, err := GetRole("dev")
	require.NoError(t, err)
	assert.Equal(t, "CUSTOM BRIEF", after.Brief, "user edit to a seed is sacred")

	// Delete a seed, then re-seed: it must be re-added.
	_, err = DeleteRole("tester")
	require.NoError(t, err)
	require.NoError(t, ensureSeededRoles(d))
	revived, err := GetRole("tester")
	require.NoError(t, err)
	require.NotNil(t, revived, "deleted seed re-added on re-seed")
}
