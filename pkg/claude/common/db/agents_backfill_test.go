package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetAgentLayer clears the freshly-migrated (empty) agent-identity tables
// and the enrollment roster so a test can stage a pre-v72 DB state with raw
// INSERTs, then drive backfillAgents over it. Mirrors the raw-insert
// technique the v30 backfill test used to dodge the Go-level triggers.
//
// agent_enrollment is dropped at head (JOH-26 PR3c v75), but backfillAgents
// still consults it WHEN PRESENT for an old DB upgrading through the chain
// (collectAgentConvs / headEnrollmentFacts guard on its existence). These unit
// tests drive backfillAgents directly on a head DB, so they re-stand-up the
// table to exercise that enrollment-source path.
func resetAgentLayer(t *testing.T, d *sql.DB) {
	t.Helper()
	mustExec(t, d, `DELETE FROM agent_conversations`)
	mustExec(t, d, `DELETE FROM agents`)
	ensureEnrollmentTableForTest(t, d)
	mustExec(t, d, `DELETE FROM agent_enrollment`)
}

// mustExec runs a statement and fails the test on error. Shared by the
// migration / backfill tests that hand-seed raw rows.
func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	_, err := d.Exec(q, args...)
	require.NoError(t, err, "exec failed: %s", q)
}

// ensureEnrollmentTableForTest recreates the v30-era agent_enrollment schema so
// a backfill test can seed it as a source. Production drops it at v75; a unit
// test that drives the v30/v72 backfill directly re-stands it up. IF NOT EXISTS
// so it composes with a DB that still has it (mid-chain).
func ensureEnrollmentTableForTest(t *testing.T, d *sql.DB) {
	t.Helper()
	mustExec(t, d, `CREATE TABLE IF NOT EXISTS agent_enrollment (
		conv_id       TEXT PRIMARY KEY,
		enrolled_at   TEXT NOT NULL,
		enrolled_via  TEXT NOT NULL DEFAULT '',
		retired_at    TEXT NOT NULL DEFAULT '',
		retired_by    TEXT NOT NULL DEFAULT '',
		retire_reason TEXT NOT NULL DEFAULT '',
		pending_name  TEXT NOT NULL DEFAULT ''
	)`)
}

// enroll raw-inserts an agent_enrollment row (bypassing the ensure path so no
// agent is auto-allocated), so a test can pin the actor facts the backfill
// must carry from the chain head.
func enroll(t *testing.T, d *sql.DB, conv, via, pendingName, retiredAt string) {
	t.Helper()
	mustExec(t, d, `INSERT INTO agent_enrollment
		(conv_id, enrolled_at, enrolled_via, retired_at, retired_by, retire_reason, pending_name)
		VALUES (?, '2020-01-01T00:00:00Z', ?, ?, '', '', ?)`,
		conv, via, retiredAt, pendingName)
}

// TestBackfillAgentsCollapsesSuccessionChain: a reincarnation chain
// (old → new, recorded as a succession edge) is ONE actor. Both generations
// resolve to the same agent_id; the actor's current conv is the chain head.
func TestBackfillAgentsCollapsesSuccessionChain(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	enroll(t, d, "old", "spawn", "", "")
	enroll(t, d, "new", "reincarnate", "", "")
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old', 'new', 'reincarnate', '2020-01-01T00:00:01Z')`)

	require.NoError(t, backfillAgents(d), "backfillAgents")

	oldAgent, err := AgentIDForConv("old")
	require.NoError(t, err)
	newAgent, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, oldAgent)
	assert.Equal(t, oldAgent, newAgent, "a replacement chain is a single actor")

	a, err := GetAgent(newAgent)
	require.NoError(t, err)
	assert.Equal(t, "new", a.CurrentConvID, "current conv is the chain head")

	// Exactly one actor for the whole chain.
	assert.Equal(t, 1, countAgents(t, d))
}

// TestBackfillAgentsMultiHopChain: a → b → c collapses to one actor with
// head c, regardless of which generation we resolve from.
func TestBackfillAgentsMultiHopChain(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	enroll(t, d, "a", "spawn", "", "")
	enroll(t, d, "b", "reincarnate", "", "")
	enroll(t, d, "c", "clear", "", "")
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('a', 'b', 'reincarnate', '2020-01-01T00:00:01Z')`)
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('b', 'c', 'clear', '2020-01-01T00:00:02Z')`)

	require.NoError(t, backfillAgents(d), "backfillAgents")

	a, _ := AgentIDForConv("a")
	b, _ := AgentIDForConv("b")
	c, _ := AgentIDForConv("c")
	assert.Equal(t, a, b)
	assert.Equal(t, b, c)
	assert.Equal(t, 1, countAgents(t, d))

	agent, _ := GetAgent(a)
	assert.Equal(t, "c", agent.CurrentConvID, "head is the end of the chain")
}

// TestBackfillAgentsKeepsClonesSeparate: a clone records NO succession edge,
// so the source and the fork are distinct actors even though both are
// enrolled. This is the load-bearing "collapse by succession edge, not by
// anything else" assertion.
func TestBackfillAgentsKeepsClonesSeparate(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	enroll(t, d, "source", "spawn", "", "")
	enroll(t, d, "fork", "clone", "", "")
	// A clone records the fork's lineage (clone history, agent-keyed since v74)
	// but there is NO succession edge between source and fork — the backfill
	// collapses ONLY along succession edges, never by lineage, so they stay
	// distinct actors.

	require.NoError(t, backfillAgents(d), "backfillAgents")

	srcAgent, _ := AgentIDForConv("source")
	forkAgent, _ := AgentIDForConv("fork")
	require.NotEmpty(t, srcAgent)
	require.NotEmpty(t, forkAgent)
	assert.NotEqual(t, srcAgent, forkAgent, "a clone is a fork — its own actor")
	assert.Equal(t, 2, countAgents(t, d))
}

// TestBackfillAgentsCarriesHeadFacts: the actor's created_via and
// pending_name come from the chain HEAD's enrollment, and a retired HEAD
// makes the actor retired.
func TestBackfillAgentsCarriesHeadFacts(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	// Head 'new' carries the live name; predecessor 'old' was retired by the
	// old MigrateAgentIdentity when it was superseded.
	enroll(t, d, "old", "spawn", "worker-old", "2020-01-02T00:00:00Z")
	enroll(t, d, "new", "reincarnate", "worker-live", "")
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old', 'new', 'reincarnate', '2020-01-01T00:00:01Z')`)

	require.NoError(t, backfillAgents(d), "backfillAgents")

	agentID, _ := AgentIDForConv("new")
	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.Equal(t, "reincarnate", a.CreatedVia, "created_via comes from the head enrollment")
	assert.Equal(t, "worker-live", a.PendingName, "pending_name comes from the head, not the predecessor")
	assert.True(t, a.Active(),
		"a predecessor's retired_at must NOT retire the actor — the head is live")
}

// TestBackfillAgentsRetiredHeadIsRetiredActor: when the human retired the
// LIVE agent (its head enrollment is retired), the actor is retired.
func TestBackfillAgentsRetiredHeadIsRetiredActor(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	enroll(t, d, "solo", "spawn", "", "2020-01-03T00:00:00Z") // retired head, no chain
	require.NoError(t, backfillAgents(d), "backfillAgents")

	agentID, _ := AgentIDForConv("solo")
	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.False(t, a.Active(), "a retired head yields a retired actor")
}

// TestBackfillAgentsCoversIdentityOnlyConv: a conv that appears only in an
// identity table (no enrollment row) still gets an actor — defensive reach.
func TestBackfillAgentsCoversIdentityOnlyConv(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	resetAgentLayer(t, d)
	// A conv that appears only in a (still conv-keyed) agentic table, with no
	// enrollment — the defensive coverage path. agent_head_aliases.anchor_conv_id
	// is one of the conv-keyed sources collectAgentConvs scans (the clone/spawn
	// history + cron tables went agent-keyed in v74, JOH-26 PR3a).
	mustExec(t, d, `INSERT INTO agent_head_aliases (handle, anchor_conv_id, created_at, by_conv)
		VALUES ('lonely-alias', 'lonely', '2020-01-01T00:00:00Z', '')`)

	require.NoError(t, backfillAgents(d), "backfillAgents")

	agentID, err := AgentIDForConv("lonely")
	require.NoError(t, err)
	assert.NotEmpty(t, agentID, "an identity-table-only conv still becomes an actor")
	a, _ := GetAgent(agentID)
	assert.Equal(t, "backfill", a.CreatedVia, "no enrollment ⇒ default created_via")
}

// TestBackfillAgentsIdempotent: a second run mints no new actors and leaves
// the mapping unchanged. This is the upgrade-safety guarantee.
func TestBackfillAgentsIdempotent(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	enroll(t, d, "old", "spawn", "", "")
	enroll(t, d, "new", "reincarnate", "", "")
	enroll(t, d, "fork", "clone", "", "")
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old', 'new', 'reincarnate', '2020-01-01T00:00:01Z')`)

	require.NoError(t, backfillAgents(d), "first backfill")
	firstCount := countAgents(t, d)
	oldAgent, _ := AgentIDForConv("old")
	forkAgent, _ := AgentIDForConv("fork")

	require.NoError(t, backfillAgents(d), "second backfill")
	assert.Equal(t, firstCount, countAgents(t, d), "re-run mints no new actors")

	oldAgent2, _ := AgentIDForConv("old")
	forkAgent2, _ := AgentIDForConv("fork")
	assert.Equal(t, oldAgent, oldAgent2, "mapping is stable across re-runs")
	assert.Equal(t, forkAgent, forkAgent2)
	assert.Equal(t, 2, firstCount, "one actor for the chain + one for the fork")
}

func countAgents(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n))
	return n
}
