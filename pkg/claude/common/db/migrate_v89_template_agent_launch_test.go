package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV88toV89_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it reaches at least v89. The literal currentVersion
// tripwire moved forward to the v90 head test (migrate_v90_group_deploy_meta_test.go).
func TestMigrateV88toV89_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.GreaterOrEqual(t, ver, 89, "fresh DB migrates through v89")
}

// TestMigrateV88toV89_AddsColumns drives the real v88→v89 ALTER over a
// v88-pinned DB: it asserts the six launch columns appear, that a pre-existing
// template agent reads them back as '' (no override), that the version
// advances, and that a re-run is a clean no-op.
func TestMigrateV88toV89_AddsColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v88 and drop the new columns so we re-add them from a true v88
	// shape (the fresh chain already ran v89). SQLite supports DROP COLUMN.
	for _, col := range []string{"spawn_profile", "harness", "model", "effort", "sandbox", "approval"} {
		mustExec(t, d, `ALTER TABLE group_template_agents DROP COLUMN `+col)
	}
	mustExec(t, d, `UPDATE schema_version SET version = 88`)

	// A pre-existing template + agent (without the new columns) must survive the
	// ALTER and read the launch fields back as the defaults.
	mustExec(t, d, `INSERT INTO group_templates
		(name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', 'ctx', '2026-07-03T00:00:00Z', '2026-07-03T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO group_template_agents
		(template_id, ordinal, name, role, descr, initial_message, is_owner, permissions)
		VALUES ((SELECT id FROM group_templates WHERE name = 'legacy'), 0, 'lead', 'r', '', '', 0, '[]')`)

	require.NoError(t, migrateV88toV89(d), "v88→v89")

	for _, col := range []string{"spawn_profile", "harness", "model", "effort", "sandbox", "approval"} {
		var n int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = ?`, col).Scan(&n))
		assert.Equalf(t, 1, n, "group_template_agents.%s added", col)
	}

	// The legacy agent reads its launch fields back through the DB layer as
	// unset — i.e. inherit the group default, today's behaviour.
	tmpl, err := GetGroupTemplate("legacy")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 1)
	a := tmpl.Agents[0]
	assert.Empty(t, a.SpawnProfile, "legacy agent has no profile reference")
	assert.Empty(t, a.Harness)
	assert.Empty(t, a.Model)
	assert.Empty(t, a.Effort)
	assert.Empty(t, a.Sandbox)
	assert.Empty(t, a.Approval)

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 89, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guards skip the duplicate ADD COLUMNs).
	require.NoError(t, migrateV88toV89(d), "v88→v89 re-run is a clean no-op")
}

// TestGroupTemplateAgent_LaunchFieldsRoundTrip proves the DB layer persists and
// reads back the per-agent launch profile through Create + Get.
func TestGroupTemplateAgent_LaunchFieldsRoundTrip(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	id, err := CreateGroupTemplate(&GroupTemplate{
		Name: "crew",
		Agents: []GroupTemplateAgent{
			{Ordinal: 0, Name: "lead", Model: "opus", Effort: "high"},
			{Ordinal: 1, Name: "tester", SpawnProfile: "cheap", Harness: "codex", Sandbox: "read-only", Approval: "never"},
		},
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	tmpl, err := GetGroupTemplate("crew")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 2)

	lead := tmpl.Agents[0]
	assert.Equal(t, "lead", lead.Name)
	assert.Equal(t, "opus", lead.Model)
	assert.Equal(t, "high", lead.Effort)
	assert.Empty(t, lead.SpawnProfile)

	tester := tmpl.Agents[1]
	assert.Equal(t, "tester", tester.Name)
	assert.Equal(t, "cheap", tester.SpawnProfile)
	assert.Equal(t, "codex", tester.Harness)
	assert.Equal(t, "read-only", tester.Sandbox)
	assert.Equal(t, "never", tester.Approval)
}
