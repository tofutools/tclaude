package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV80toV81_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts every v81 companion column is present. v81 is head, so the
// literal currentVersion tripwire lives here now (moved forward from v80).
func TestMigrateV80toV81_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 81, currentVersion, "tripwire: bump this and add a v81→v82 test when you add a migration")

	for _, spec := range v81CompanionAgentColumns {
		has, err := columnExists(d, spec.table, spec.agentCol)
		require.NoError(t, err, "columnExists %s.%s", spec.table, spec.agentCol)
		assert.Truef(t, has, "%s carries the agent companion column %s", spec.table, spec.agentCol)
	}
}

// TestMigrateV80toV81_BackfillsCompanionAgents drives the real v80→v81 migration
// over hand-seeded v80-shaped rows: every non-empty conv ref must resolve to that
// conv's owning actor, an empty conv ref (human-initiated path) must stay ”, and
// a re-run must change nothing.
func TestMigrateV80toV81_BackfillsCompanionAgents(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	agentSender, _, err := EnsureAgentForConv("senderConv", "spawn")
	require.NoError(t, err, "EnsureAgentForConv senderConv")
	agentReply, _, err := EnsureAgentForConv("replyConv", "spawn")
	require.NoError(t, err, "EnsureAgentForConv replyConv")
	agentSpawner, _, err := EnsureAgentForConv("spawnerConv", "spawn")
	require.NoError(t, err, "EnsureAgentForConv spawnerConv")

	// Reshape the tables-under-test back to their v80 (pre-companion) form and pin
	// the version, then seed rows in that shape.
	for _, s := range []string{
		`ALTER TABLE human_messages DROP COLUMN from_agent`,
		`ALTER TABLE pending_spawns DROP COLUMN reply_to_agent`,
		`ALTER TABLE pending_spawns DROP COLUMN spawned_by_agent`,
		`UPDATE schema_version SET version = 80`,
	} {
		mustExec(t, d, s)
	}

	// human_messages: an agent-sent notification + a human-initiated one (empty
	// from_conv, e.g. the worktree-cleanup system message).
	mustExec(t, d, `INSERT INTO human_messages (id, from_conv, body, created_at)
		VALUES (1, 'senderConv', 'hi', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO human_messages (id, from_conv, body, created_at)
		VALUES (2, '', 'system', '2020-01-02T00:00:00Z')`)
	// pending_spawns: an agent-initiated spawn (reply-to + spawned-by both actors)
	// and a human-initiated one (both empty).
	mustExec(t, d, `INSERT INTO pending_spawns (label, group_id, reply_to_conv, spawned_by_conv, created_at)
		VALUES ('lbl-agent', 1, 'replyConv', 'spawnerConv', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO pending_spawns (label, group_id, reply_to_conv, spawned_by_conv, created_at)
		VALUES ('lbl-human', 1, '', '', '2020-01-02T00:00:00Z')`)

	require.NoError(t, migrateV80toV81(d), "v80→v81 backfill")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 81, ver, "version advanced")

	var fromAgent string
	require.NoError(t, d.QueryRow(`SELECT from_agent FROM human_messages WHERE id = 1`).Scan(&fromAgent))
	assert.Equal(t, agentSender, fromAgent, "human message sender conv backfills to its actor")
	require.NoError(t, d.QueryRow(`SELECT from_agent FROM human_messages WHERE id = 2`).Scan(&fromAgent))
	assert.Equal(t, "", fromAgent, "human-initiated message (empty from_conv) leaves from_agent empty")

	var replyAgent, spawnedByAgent string
	require.NoError(t, d.QueryRow(
		`SELECT reply_to_agent, spawned_by_agent FROM pending_spawns WHERE label = 'lbl-agent'`,
	).Scan(&replyAgent, &spawnedByAgent))
	assert.Equal(t, agentReply, replyAgent, "reply_to_conv backfills to its actor")
	assert.Equal(t, agentSpawner, spawnedByAgent, "spawned_by_conv backfills to its actor")

	require.NoError(t, d.QueryRow(
		`SELECT reply_to_agent, spawned_by_agent FROM pending_spawns WHERE label = 'lbl-human'`,
	).Scan(&replyAgent, &spawnedByAgent))
	assert.Equal(t, "", replyAgent, "human-initiated spawn leaves reply_to_agent empty")
	assert.Equal(t, "", spawnedByAgent, "human-initiated spawn leaves spawned_by_agent empty")

	// Idempotent: a re-run recomputes the same join and changes nothing.
	require.NoError(t, migrateV80toV81(d), "v80→v81 re-run is a clean no-op")
	require.NoError(t, d.QueryRow(`SELECT from_agent FROM human_messages WHERE id = 1`).Scan(&fromAgent))
	assert.Equal(t, agentSender, fromAgent, "re-run leaves the backfilled agent intact")
}

// TestDualWrite_HumanMessage pins the insert-time dual-write: a freshly recorded
// human message has from_agent DERIVED from from_conv, with a non-actor / empty
// sender left ”. ListHumanMessages surfaces the companion on read.
func TestDualWrite_HumanMessage(t *testing.T) {
	setupTestDB(t)

	sender, _, err := EnsureAgentForConv("senderConv", "spawn")
	require.NoError(t, err)

	id, err := InsertHumanMessage(&HumanMessage{FromConv: "senderConv", Body: "hi"})
	require.NoError(t, err, "InsertHumanMessage")

	d, err := Open()
	require.NoError(t, err)
	var fromAgent string
	require.NoError(t, d.QueryRow(`SELECT from_agent FROM human_messages WHERE id = ?`, id).Scan(&fromAgent))
	assert.Equal(t, sender, fromAgent, "from_agent dual-written from from_conv")

	// A non-actor sender leaves from_agent ''.
	_, err = InsertHumanMessage(&HumanMessage{FromConv: "not-an-agent", Body: "x"})
	require.NoError(t, err)

	// ListHumanMessages surfaces the companion (newest first).
	msgs, err := ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "", msgs[0].FromAgent, "non-actor sender surfaces empty FromAgent")
	assert.Equal(t, sender, msgs[1].FromAgent, "actor sender surfaces its agent_id on read")
}

// TestDualWrite_PendingSpawn pins the insert-time dual-write: a freshly recorded
// pending spawn derives reply_to_agent / spawned_by_agent from its conv columns,
// surfaced by Get/ListPendingSpawns; an empty (human-initiated) conv stays ”.
func TestDualWrite_PendingSpawn(t *testing.T) {
	setupTestDB(t)

	reply, _, err := EnsureAgentForConv("replyConv", "spawn")
	require.NoError(t, err)
	spawner, _, err := EnsureAgentForConv("spawnerConv", "spawn")
	require.NoError(t, err)

	require.NoError(t, InsertPendingSpawn(&PendingSpawn{
		Label: "lbl-agent", GroupID: 1,
		ReplyToConv: "replyConv", SpawnedByConv: "spawnerConv",
	}))
	require.NoError(t, InsertPendingSpawn(&PendingSpawn{
		Label: "lbl-human", GroupID: 1,
	}))

	got, err := GetPendingSpawn("lbl-agent")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, reply, got.ReplyToAgent, "reply_to_agent dual-written from reply_to_conv")
	assert.Equal(t, spawner, got.SpawnedByAgent, "spawned_by_agent dual-written from spawned_by_conv")

	human, err := GetPendingSpawn("lbl-human")
	require.NoError(t, err)
	require.NotNil(t, human)
	assert.Equal(t, "", human.ReplyToAgent, "human-initiated spawn leaves reply_to_agent empty")
	assert.Equal(t, "", human.SpawnedByAgent, "human-initiated spawn leaves spawned_by_agent empty")
}
