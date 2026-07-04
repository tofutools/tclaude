package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV92toV93_AddsChoreography drives the real v92→v93 migration over a
// v92-pinned DB: the wave / rhythms / wave_max_wait / target_role columns
// appear, the choreography table appears, a pre-existing template + template
// agent + cron job read the new fields back as their zero values, the version
// advances, and a re-run is a clean no-op.
func TestMigrateV92toV93_AddsChoreography(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v92: drop the new columns + table so we re-create them from a
	// true v92 shape (the fresh chain already ran v93).
	mustExec(t, d, `ALTER TABLE group_template_agents DROP COLUMN wave`)
	mustExec(t, d, `ALTER TABLE group_templates DROP COLUMN rhythms`)
	mustExec(t, d, `ALTER TABLE group_templates DROP COLUMN wave_max_wait`)
	mustExec(t, d, `ALTER TABLE agent_cron_jobs DROP COLUMN target_role`)
	mustExec(t, d, `DROP TABLE IF EXISTS group_wave_choreography`)
	mustExec(t, d, `UPDATE schema_version SET version = 92`)

	// A pre-existing template + agent (without the new fields) must survive the
	// ALTERs.
	mustExec(t, d, `INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		VALUES ('legacy', 'd', '', '2026-07-04T00:00:00Z', '2026-07-04T00:00:00Z')`)
	var tid int64
	require.NoError(t, d.QueryRow(`SELECT id FROM group_templates WHERE name = 'legacy'`).Scan(&tid))
	mustExec(t, d, `INSERT INTO group_template_agents (template_id, ordinal, name) VALUES (?, 0, 'lead')`, tid)
	mustExec(t, d, `INSERT INTO agent_cron_jobs (name, owner_agent, target_agent, interval_seconds, created_at) VALUES ('legacy-job', 'agt_x', 'agt_y', 600, '2026-07-04T00:00:00Z')`)

	require.NoError(t, migrateV92toV93(d), "v92→v93")

	// Each new column exists.
	for _, tc := range []struct{ table, col string }{
		{"group_template_agents", "wave"},
		{"group_templates", "rhythms"},
		{"group_templates", "wave_max_wait"},
		{"agent_cron_jobs", "target_role"},
	} {
		var have int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, tc.table, tc.col).Scan(&have),
			"probe %s.%s", tc.table, tc.col)
		assert.Equalf(t, 1, have, "%s.%s added", tc.table, tc.col)
	}

	// The choreography table exists.
	var haveTbl int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'group_wave_choreography'`).Scan(&haveTbl))
	assert.Equal(t, 1, haveTbl, "group_wave_choreography table created")

	// The legacy rows read the new fields back as their zero values.
	var wave int
	require.NoError(t, d.QueryRow(`SELECT wave FROM group_template_agents WHERE name = 'lead'`).Scan(&wave))
	assert.Equal(t, 0, wave, "legacy agent has wave 0")
	var rhythms string
	var maxWait int
	require.NoError(t, d.QueryRow(`SELECT rhythms, wave_max_wait FROM group_templates WHERE name = 'legacy'`).Scan(&rhythms, &maxWait))
	assert.Empty(t, rhythms, "legacy template has no rhythms")
	assert.Equal(t, 0, maxWait, "legacy template has default wave_max_wait")
	var role string
	require.NoError(t, d.QueryRow(`SELECT target_role FROM agent_cron_jobs WHERE name = 'legacy-job'`).Scan(&role))
	assert.Empty(t, role, "legacy cron job has no role filter")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 93, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op.
	require.NoError(t, migrateV92toV93(d), "v92→v93 re-run is a clean no-op")
}
