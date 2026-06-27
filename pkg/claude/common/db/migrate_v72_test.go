package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV71toV72_FreshSchema builds a fresh DB through the full
// migrate() chain and asserts the stable agent-identity layer exists. v72 is
// head, so this is where the literal currentVersion tripwire now lives — the
// next migration's author moves it forward into their own v73 test.
func TestMigrateV71toV72_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	// (the literal currentVersion tripwire moved forward to the v73 test)

	for _, table := range []string{"agents", "agent_conversations"} {
		var n int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n))
		assert.Equal(t, 1, n, "fresh schema has %s", table)
	}
}

// TestMigrateV71toV72_BackfillsExistingAgents drives the migration over a
// realistic pre-v72 state: an active agent and a reincarnation chain, staged
// with raw INSERTs, then runs migrateV71toV72 and confirms the actor layer
// reflects it. Locks the migration's wiring to the (separately unit-tested)
// backfill.
func TestMigrateV71toV72_BackfillsExistingAgents(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Clear the (empty) layer the fresh migration produced and stage a
	// pre-v72 state, then re-run the migration's backfill path.
	resetAgentLayer(t, d)
	enroll(t, d, "solo", "spawn", "solo-name", "")
	enroll(t, d, "old", "spawn", "", "")
	enroll(t, d, "new", "reincarnate", "live-name", "")
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old', 'new', 'reincarnate', '2020-01-01T00:00:01Z')`)
	mustExec(t, d, `UPDATE schema_version SET version = 71`)

	require.NoError(t, migrateV71toV72(d), "re-run migration backfills existing agents")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 72, ver)

	// solo is its own actor; old+new collapse to one.
	solo, _ := AgentIDForConv("solo")
	oldA, _ := AgentIDForConv("old")
	newA, _ := AgentIDForConv("new")
	require.NotEmpty(t, solo)
	require.NotEmpty(t, oldA)
	assert.Equal(t, oldA, newA, "the reincarnation chain is one actor")
	assert.NotEqual(t, solo, oldA, "the standalone agent is distinct")
	assert.Equal(t, 2, countAgents(t, d))

	a, _ := GetAgent(newA)
	assert.Equal(t, "new", a.CurrentConvID, "current conv is the chain head")
	assert.Equal(t, "live-name", a.PendingName)
}

// TestMigrateV71toV72_HealsMissingAgenticTables: a partial-schema DB (no
// agentic tables at all) migrates to head without tripping on a missing
// table in the backfill. Mirrors the v56/v61 heal tests for this migration.
func TestMigrateV71toV72_HealsMissingAgenticTables(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// A bare DB carrying only version + sessions, pinned below v72.
	mustExec(t, d, `DELETE FROM agent_conversations`)
	mustExec(t, d, `DELETE FROM agents`)
	mustExec(t, d, `DROP TABLE IF EXISTS agents`)
	mustExec(t, d, `DROP TABLE IF EXISTS agent_conversations`)
	mustExec(t, d, `DROP TABLE IF EXISTS agent_conv_succession`)
	mustExec(t, d, `DROP TABLE IF EXISTS agent_enrollment`)
	mustExec(t, d, `UPDATE schema_version SET version = 71`)

	require.NoError(t, migrateV71toV72(d), "backfill must skip missing source tables, not error")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 72, ver)
}
