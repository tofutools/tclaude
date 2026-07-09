package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV101toV102_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts it lands at currentVersion. v102 is head, so the
// literal currentVersion tripwire lives here.
func TestMigrateV101toV102_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 102, currentVersion, "tripwire: bump this and add a v102->v103 test when you add a migration")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = 'profile_inline'`).Scan(&have))
	assert.Equal(t, 1, have, "fresh schema has group_template_agents.profile_inline")
}

// TestMigrateV101toV102_AddsColumn drives the real v101→v102 ALTER over a
// v101-pinned DB: the profile_inline column appears, a pre-existing template
// agent reads it back as nil (no inline profile), the version advances, and a
// re-run is a clean no-op.
func TestMigrateV101toV102_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v101 and drop the new column so we re-add it from a true v101
	// shape (the fresh chain already ran v102).
	mustExec(t, d, `ALTER TABLE group_template_agents DROP COLUMN profile_inline`)
	mustExec(t, d, `UPDATE schema_version SET version = 101`)

	mustExec(t, d, `INSERT INTO group_templates
		(name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', 'ctx', '2026-07-09T00:00:00Z', '2026-07-09T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO group_template_agents
		(template_id, ordinal, name, role, descr, initial_message, is_owner, permissions)
		VALUES ((SELECT id FROM group_templates WHERE name = 'legacy'), 0, 'lead', 'r', '', '', 0, '[]')`)

	require.NoError(t, migrateV101toV102(d), "v101→v102")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = 'profile_inline'`).Scan(&n))
	assert.Equal(t, 1, n, "group_template_agents.profile_inline added")

	tmpl, err := GetGroupTemplate("legacy")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 1)
	assert.Nil(t, tmpl.Agents[0].ProfileInline, "legacy agent has no inline profile")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 102, ver, "version advanced")

	require.NoError(t, migrateV101toV102(d), "v101→v102 re-run is a clean no-op")
}

// TestGroupTemplateAgent_ProfileInlineRoundTrip proves the DB layer persists
// and reads back a template-local spawn profile through Create + Get, and that
// Update replaces it (including clearing it back to nil).
func TestGroupTemplateAgent_ProfileInlineRoundTrip(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	tr := true
	inline := &SpawnProfile{
		Harness:                "codex",
		Model:                  "gpt-5.5",
		Effort:                 "high",
		Sandbox:                "tclaude-agent",
		AskUserQuestionTimeout: "",
		TrustDir:               &tr,
		IsOwner:                &tr,
		PermissionOverrides:    map[string]string{"templates.manage": "grant", "human.notify": "deny"},
	}
	id, err := CreateGroupTemplate(&GroupTemplate{
		Name: "crew",
		Agents: []GroupTemplateAgent{
			{Ordinal: 0, Name: "lead", ProfileInline: inline},
			{Ordinal: 1, Name: "dev"},
		},
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := GetGroupTemplate("crew")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Agents, 2)

	lead := got.Agents[0]
	require.NotNil(t, lead.ProfileInline, "inline profile round-trips")
	assert.Equal(t, "codex", lead.ProfileInline.Harness)
	assert.Equal(t, "gpt-5.5", lead.ProfileInline.Model)
	assert.Equal(t, "high", lead.ProfileInline.Effort)
	assert.Equal(t, "tclaude-agent", lead.ProfileInline.Sandbox)
	require.NotNil(t, lead.ProfileInline.TrustDir)
	assert.True(t, *lead.ProfileInline.TrustDir)
	require.NotNil(t, lead.ProfileInline.IsOwner)
	assert.True(t, *lead.ProfileInline.IsOwner)
	assert.Equal(t, map[string]string{"templates.manage": "grant", "human.notify": "deny"},
		lead.ProfileInline.PermissionOverrides)
	assert.Nil(t, got.Agents[1].ProfileInline, "agent without inline profile stays nil")

	// Full-replace update clears it.
	got.Agents[0].ProfileInline = nil
	require.NoError(t, UpdateGroupTemplate(got))
	got2, err := GetGroupTemplate("crew")
	require.NoError(t, err)
	assert.Nil(t, got2.Agents[0].ProfileInline, "update clears the inline profile")
}
