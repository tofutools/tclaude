package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV73toV74_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts the cron / spawn / clone tables came out agent-keyed. v74 is
// head, so the literal currentVersion tripwire lives here now (moved forward
// from the v73 test); the next migration's author moves it into their own test.
func TestMigrateV73toV74_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 74, currentVersion, "tripwire: bump this and add a v74→v75 test when you add a migration")

	// The cutover renamed each conv ref to its agent-keyed name.
	for _, c := range []struct{ table, agentCol, convCol string }{
		{"agent_cron_jobs", "owner_agent", "owner_conv"},
		{"agent_cron_jobs", "target_agent", "target_conv"},
		{"agent_spawn_history", "spawner_agent_id", "spawner_conv_id"},
		{"agent_clone_history", "source_agent_id", "source_conv_id"},
	} {
		hasAgent, err := columnExists(d, c.table, c.agentCol)
		require.NoError(t, err)
		assert.True(t, hasAgent, "%s is agent-keyed (%s)", c.table, c.agentCol)
		hasConv, err := columnExists(d, c.table, c.convCol)
		require.NoError(t, err)
		assert.False(t, hasConv, "%s no longer carries %s", c.table, c.convCol)
	}
}

// TestMigrateV73toV74_TransformsRefsAndPreservesRuns drives the real v73→v74
// cutover over hand-seeded conv-keyed rows: it asserts every owner/target/
// spawner/source ref is rewritten from its conv to that conv's owning actor, that
// two generations of one actor collapse onto the same agent key (so a rate limit
// follows the actor), and — crucially — that the cron run history SURVIVES (the
// RENAME COLUMN approach never drops agent_cron_jobs, so the agent_cron_runs FK
// never cascade-deletes).
func TestMigrateV73toV74_TransformsRefsAndPreservesRuns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// One actor with two generations: g0 (predecessor) → g1 (current).
	agentA, err := AllocateAgent("g1", "spawn")
	require.NoError(t, err, "AllocateAgent g1")
	require.NoError(t, LinkConvToAgent("g0", agentA, ConvRoleGeneration, "test"), "link g0")

	// A separate actor that owns/targets a cron job.
	agentB, err := AllocateAgent("mgr", "spawn")
	require.NoError(t, err, "AllocateAgent mgr")

	seedV73ConvKeyedCronHistory(t, d)

	// A conv-target cron job owned by mgr, targeting g1, with a run row.
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_conv, target_kind, target_conv, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES ('job', 'mgr', 'conv', 'g1', 0, 600, '', 'ping', 1, '2020-01-01T00:00:00Z', '', '')`)
	var jobID int64
	require.NoError(t, d.QueryRow(`SELECT id FROM agent_cron_jobs WHERE name = 'job'`).Scan(&jobID))
	mustExec(t, d, `INSERT INTO agent_cron_runs (job_id, fired_at, status, error_msg)
		VALUES (?, '2020-01-01T00:10:00Z', 'ok', '')`, jobID)

	// A group-target job: owner-less (human-scheduled), no target conv.
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_conv, target_kind, target_conv, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES ('grp', '', 'group', '', 7, 600, '', 'team', 1, '2020-01-01T00:00:00Z', '', '')`)

	// Spawn history under BOTH generations of actor A — the rate-limit subject
	// that must collapse onto one agent key after the cutover.
	mustExec(t, d, `INSERT INTO agent_spawn_history (spawner_conv_id, spawned_at)
		VALUES ('g0', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_spawn_history (spawner_conv_id, spawned_at)
		VALUES ('g1', '2020-01-02T00:00:00Z')`)
	// Clone history keyed on mgr.
	mustExec(t, d, `INSERT INTO agent_clone_history (source_conv_id, cloned_at)
		VALUES ('mgr', '2020-01-01T00:00:00Z')`)

	require.NoError(t, migrateV73toV74(d), "v73→v74 cutover")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 74, ver, "version advanced")

	// Cron job refs rewritten to agents; the group job stays owner-less/targetless.
	var ownerAgent, targetAgent string
	require.NoError(t, d.QueryRow(
		`SELECT owner_agent, target_agent FROM agent_cron_jobs WHERE name = 'job'`).Scan(&ownerAgent, &targetAgent))
	assert.Equal(t, agentB, ownerAgent, "owner conv → owner agent")
	assert.Equal(t, agentA, targetAgent, "target conv → target agent")
	var grpOwner, grpTarget string
	require.NoError(t, d.QueryRow(
		`SELECT owner_agent, target_agent FROM agent_cron_jobs WHERE name = 'grp'`).Scan(&grpOwner, &grpTarget))
	assert.Equal(t, "", grpOwner, "owner-less group job stays empty")
	assert.Equal(t, "", grpTarget, "group job carries no target agent")

	// The run history survived the column rename (no FK cascade-delete).
	var runs int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_cron_runs`).Scan(&runs))
	assert.Equal(t, 1, runs, "cron run history survives the cutover")

	// Both spawn rows now key on actor A; the read API resolves either
	// generation's conv to the same actor and counts both (rate limit follows
	// the actor across rotations).
	var spawnUnderA int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM agent_spawn_history WHERE spawner_agent_id = ?`, agentA).Scan(&spawnUnderA))
	assert.Equal(t, 2, spawnUnderA, "both generations' spawns collapse onto one actor")

	// Clone ref rewritten to mgr's actor.
	var cloneAgent string
	require.NoError(t, d.QueryRow(
		`SELECT source_agent_id FROM agent_clone_history LIMIT 1`).Scan(&cloneAgent))
	assert.Equal(t, agentB, cloneAgent, "source conv → source agent")

	// Live read surface: the cron job resolves owner/target agent back to the
	// actor's CURRENT conv.
	j, err := GetAgentCronJob(jobID)
	require.NoError(t, err)
	require.NotNil(t, j)
	assert.Equal(t, "mgr", j.OwnerConv, "owner resolves to current conv")
	assert.Equal(t, "g1", j.TargetConv, "target resolves to current conv")
}

// TestUnmappedV74Rows_DetectsOrphan checks the strict coverage gate: it counts
// non-empty refs whose conv has no agent_conversations mapping (the refs the
// transform would blank). A mapped conv is fine; an unmapped one is reported so
// migrateV73toV74 can abort instead of de-targeting a job.
func TestUnmappedV74Rows_DetectsOrphan(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	seedV73ConvKeyedCronHistory(t, d)
	resetAgentLayer(t, d) // start from a clean actor layer

	// Map 'mapped'; leave 'orphan' deliberately unmapped.
	agentID := newAgentID()
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at, created_via)
		VALUES (?, 'mapped', '2020-01-01T00:00:00Z', 'test')`, agentID)
	mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, role, reason, linked_at)
		VALUES ('mapped', ?, 'head', 'test', '2020-01-01T00:00:00Z')`, agentID)

	// A job owned by the mapped conv but targeting the orphan; an owner-less
	// group job ('' refs) must NOT be reported.
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_conv, target_kind, target_conv, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES ('j', 'mapped', 'conv', 'orphan', 0, 600, '', 'b', 1, '2020-01-01T00:00:00Z', '', '')`)
	mustExec(t, d, `INSERT INTO agent_cron_jobs
		(name, owner_conv, target_kind, target_conv, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES ('g', '', 'group', '', 7, 600, '', 'b', 1, '2020-01-01T00:00:00Z', '', '')`)
	mustExec(t, d, `INSERT INTO agent_spawn_history (spawner_conv_id, spawned_at)
		VALUES ('mapped', '2020-01-01T00:00:00Z')`)

	unmapped, err := unmappedV74Rows(d)
	require.NoError(t, err)
	assert.Equal(t, 1, unmapped["agent_cron_jobs.target_conv"], "the orphan target is unmapped")
	assert.NotContains(t, unmapped, "agent_cron_jobs.owner_conv", "the mapped owner is fine")
	assert.NotContains(t, unmapped, "agent_spawn_history.spawner_conv_id", "the mapped spawner is fine")
}

// seedV73ConvKeyedCronHistory reshapes the head (v74, agent-keyed) cron / spawn /
// clone tables back to the v73 conv-keyed shape and pins the version to 73 — so a
// test can drive the real v73→v74 cutover (or exercise its coverage gate) over
// hand-seeded conv-keyed rows. RENAME COLUMN (not DROP) keeps the agent_cron_runs
// FK intact, mirroring the migration itself; the tables are empty in a fresh DB.
func seedV73ConvKeyedCronHistory(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, s := range []string{
		`ALTER TABLE agent_cron_jobs RENAME COLUMN owner_agent TO owner_conv`,
		`ALTER TABLE agent_cron_jobs RENAME COLUMN target_agent TO target_conv`,
		`ALTER TABLE agent_spawn_history RENAME COLUMN spawner_agent_id TO spawner_conv_id`,
		`ALTER TABLE agent_clone_history RENAME COLUMN source_agent_id TO source_conv_id`,
		`UPDATE schema_version SET version = 73`,
	} {
		mustExec(t, d, s)
	}
}
