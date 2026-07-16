package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV75toV76_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts agent_messages came out with the actor columns. (The
// literal currentVersion tripwire moved forward to the v77 test, which is now
// head.)
func TestMigrateV75toV76_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	for _, col := range []string{"from_agent", "to_agent"} {
		has, err := columnExists(d, "agent_messages", col)
		require.NoError(t, err)
		assert.True(t, has, "agent_messages carries the actor column %s", col)
	}
}

// TestMigrateV75toV76_BackfillsAgentRefs drives the real v75→v76 migration over
// hand-seeded v75-shaped message rows: every non-empty conv ref must be rewritten
// to that conv's owning actor, an unmapped/non-actor conv must stay ”, and a
// second generation of one actor must resolve to the SAME agent_id (proving the
// stable identity the whole JOH-27 line is about). The migration is also re-run
// to confirm it is idempotent.
func TestMigrateV75toV76_BackfillsAgentRefs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Two actors. actorA spans two generations (g0 → g1, the head); actorB is a
	// single conv. 'plain' is a conv that was never an agent.
	agentB, _, err := EnsureAgentForConv("convB", "spawn")
	require.NoError(t, err, "EnsureAgentForConv convB")
	agentA, _, err := EnsureAgentForConv("g1", "spawn")
	require.NoError(t, err, "EnsureAgentForConv g1")
	require.NoError(t, LinkConvToAgent("g0", agentA, ConvRoleGeneration, "test"), "link g0")

	// Reshape agent_messages back to its v75 (pre-actor-column) form and pin the
	// version, then seed rows in that shape.
	seedV75MessagesWithoutAgentCols(t, d)

	// A message from a predecessor generation to actorB; one from actorB back to
	// the head generation; one to a plain (non-actor) conv.
	mustExec(t, d, `INSERT INTO agent_messages (group_id, from_conv, to_conv, created_at)
		VALUES (0, 'g0', 'convB', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_messages (group_id, from_conv, to_conv, created_at)
		VALUES (0, 'convB', 'g1', '2020-01-02T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_messages (group_id, from_conv, to_conv, created_at)
		VALUES (0, 'convB', 'plain', '2020-01-03T00:00:00Z')`)

	require.NoError(t, migrateV75toV76(d), "v75→v76 backfill")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 76, ver, "version advanced")

	// from_conv g0 (a predecessor of actorA) backfills to actorA's stable id.
	fromAgent, toAgent := mustMsgAgents(t, d, "g0", "convB")
	assert.Equal(t, agentA, fromAgent, "predecessor conv backfills to its actor")
	assert.Equal(t, agentB, toAgent, "recipient conv backfills to its actor")

	// The reply targets the head generation g1 — same actor as g0, so to_agent
	// equals the SAME agentA. This is the stable-identity property.
	fromAgent, toAgent = mustMsgAgents(t, d, "convB", "g1")
	assert.Equal(t, agentB, fromAgent)
	assert.Equal(t, agentA, toAgent, "head generation resolves to the same actor as its predecessor")

	// A non-actor recipient stays '' — blanking is correct, not data loss.
	_, toAgent = mustMsgAgents(t, d, "convB", "plain")
	assert.Equal(t, "", toAgent, "non-actor conv leaves the actor column empty")

	// Idempotent: a re-run recomputes the same join and changes nothing.
	require.NoError(t, migrateV75toV76(d), "v75→v76 re-run is a clean no-op")
	fromAgent, toAgent = mustMsgAgents(t, d, "g0", "convB")
	assert.Equal(t, agentA, fromAgent, "re-run leaves from_agent intact")
	assert.Equal(t, agentB, toAgent, "re-run leaves to_agent intact")
}

// TestInsertAgentMessage_DualWritesAgentRefs pins the send-path dual-write: a
// freshly inserted message has from_agent/to_agent DERIVED from its conv columns
// (the same join the backfill used), with a non-actor conv leaving ”. A value
// preset on the struct is ignored — the conv columns are the source of truth.
func TestInsertAgentMessage_DualWritesAgentRefs(t *testing.T) {
	setupTestDB(t)

	agentFrom, _, err := EnsureAgentForConv("sender", "spawn")
	require.NoError(t, err, "EnsureAgentForConv sender")
	agentTo, _, err := EnsureAgentForConv("recipient", "spawn")
	require.NoError(t, err, "EnsureAgentForConv recipient")

	id, err := InsertAgentMessage(&AgentMessage{
		FromConv: "sender",
		ToConv:   "recipient",
		// Deliberately bogus presets — must be ignored, derived from the convs.
		FromAgent: "agt_bogusfromvalue",
		ToAgent:   "agt_bogustovalue",
		Subject:   "hi",
		Body:      "test",
	})
	require.NoError(t, err, "InsertAgentMessage")

	m, err := GetAgentMessage(id)
	require.NoError(t, err, "GetAgentMessage")
	require.NotNil(t, m)
	assert.Equal(t, agentFrom, m.FromAgent, "from_agent derived from from_conv (preset ignored)")
	assert.Equal(t, agentTo, m.ToAgent, "to_agent derived from to_conv (preset ignored)")

	// A message to a conv that is not an actor leaves to_agent ''.
	id2, err := InsertAgentMessage(&AgentMessage{
		FromConv: "sender",
		ToConv:   "not-an-agent",
		Subject:  "x",
	})
	require.NoError(t, err, "InsertAgentMessage non-actor")
	m2, err := GetAgentMessage(id2)
	require.NoError(t, err, "GetAgentMessage non-actor")
	require.NotNil(t, m2)
	assert.Equal(t, agentFrom, m2.FromAgent, "from_agent still derived")
	assert.Equal(t, "", m2.ToAgent, "non-actor recipient leaves to_agent empty")
}

// seedV75MessagesWithoutAgentCols reshapes the head (v76) agent_messages table
// back to its v75 form — dropping from_agent / to_agent — and pins the version to
// 75, so a test can drive the real v75→v76 backfill over hand-seeded v75-shaped
// rows. Mirrors seedV73ConvKeyedCronHistory. The table is empty in a fresh DB, so
// the DROP COLUMNs never lose data here.
func seedV75MessagesWithoutAgentCols(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, s := range []string{
		// Drop the v80 index first: it references to_agent, so SQLite refuses
		// to drop the column while it exists. At v75 the index didn't exist.
		`DROP INDEX IF EXISTS idx_agent_messages_to_agent`,
		`DROP INDEX IF EXISTS idx_agent_messages_regular_agent_backlog`,
		`ALTER TABLE agent_messages DROP COLUMN from_agent`,
		`ALTER TABLE agent_messages DROP COLUMN to_agent`,
		`UPDATE schema_version SET version = 75`,
	} {
		mustExec(t, d, s)
	}
}

// mustMsgAgents reads the (from_agent, to_agent) of the single message matching
// the given conv pair, failing the test if it is missing. Used to assert the
// backfill result at the column level (no struct round-trip).
func mustMsgAgents(t *testing.T, d *sql.DB, fromConv, toConv string) (string, string) {
	t.Helper()
	var fromAgent, toAgent string
	require.NoError(t, d.QueryRow(
		`SELECT from_agent, to_agent FROM agent_messages WHERE from_conv = ? AND to_conv = ?`,
		fromConv, toConv).Scan(&fromAgent, &toAgent), "read message agents")
	return fromAgent, toAgent
}
