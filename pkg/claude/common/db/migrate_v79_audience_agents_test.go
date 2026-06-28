package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV78toV79_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts the audience-agent companion columns are present. The
// literal currentVersion tripwire moved forward to the v80 test (head).
func TestMigrateV78toV79_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	for _, col := range []string{"to_recipient_agents", "cc_recipient_agents"} {
		has, err := columnExists(d, "agent_messages", col)
		require.NoError(t, err, "columnExists agent_messages.%s", col)
		assert.True(t, has, "agent_messages carries the %s companion column", col)
	}
}

// TestMigrateV78toV79_BackfillsAudienceAgents drives the real v78→v79 migration
// over hand-seeded v78-shaped agent_messages rows. A message whose audience
// (to_recipients / cc_recipients) names enrolled convs backfills the parallel
// agent arrays, indexed 1:1: an enrolled recipient resolves to its stable actor
// id, a non-actor recipient stays "" in its slot, a row with no audience stays
// empty, and a re-run changes nothing. This pins the same agent_conversations
// derivation the send path (InsertAgentMessage) dual-writes, so backfilled and
// freshly-inserted rows agree.
func TestMigrateV78toV79_BackfillsAudienceAgents(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Enroll the recipients that are actors; "cc2-conv" is deliberately left
	// unenrolled so it exercises the non-actor "" slot.
	primary, err := AllocateAgent("primary-conv", "spawn")
	require.NoError(t, err)
	cc1, err := AllocateAgent("cc1-conv", "spawn")
	require.NoError(t, err)

	// Reshape agent_messages back to its v78 (pre-audience-agent) form and pin the
	// version, then seed the audience in that shape with raw INSERTs (the companion
	// columns are gone, so InsertAgentMessage can't be used here).
	for _, s := range []string{
		`ALTER TABLE agent_messages DROP COLUMN to_recipient_agents`,
		`ALTER TABLE agent_messages DROP COLUMN cc_recipient_agents`,
		`UPDATE schema_version SET version = 78`,
	} {
		mustExec(t, d, s)
	}
	mustExec(t, d, `INSERT INTO agent_messages
		(id, group_id, from_conv, to_conv, subject, body, created_at, to_recipients, cc_recipients)
		VALUES (1, 0, 'sender-conv', 'primary-conv', '', 'hi', '2020-01-01T00:00:00Z',
		 '["primary-conv"]', '["cc1-conv","cc2-conv"]')`)
	// A legacy single-recipient row with no audience stays empty.
	mustExec(t, d, `INSERT INTO agent_messages
		(id, group_id, from_conv, to_conv, subject, body, created_at)
		VALUES (2, 0, 'sender-conv', 'lone-conv', '', 'old', '2020-01-02T00:00:00Z')`)

	require.NoError(t, migrateV78toV79(d), "v78→v79 backfill")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 79, ver, "version advanced")

	var toA, ccA string
	require.NoError(t, d.QueryRow(`SELECT to_recipient_agents, cc_recipient_agents FROM agent_messages WHERE id = 1`).Scan(&toA, &ccA))
	assert.Equal(t, recipientsToJSON([]string{primary}), toA, "to_recipient_agents backfilled to the actor id")
	assert.Equal(t, recipientsToJSON([]string{cc1, ""}), ccA, "cc array keeps 1:1 slots; a non-actor recipient stays empty")

	require.NoError(t, d.QueryRow(`SELECT to_recipient_agents, cc_recipient_agents FROM agent_messages WHERE id = 2`).Scan(&toA, &ccA))
	assert.Equal(t, "", toA, "no-audience row leaves to_recipient_agents empty")
	assert.Equal(t, "", ccA, "no-audience row leaves cc_recipient_agents empty")

	// Idempotent: a re-run recomputes the same resolution and changes nothing.
	require.NoError(t, migrateV78toV79(d), "v78→v79 re-run is a clean no-op")
	require.NoError(t, d.QueryRow(`SELECT to_recipient_agents FROM agent_messages WHERE id = 1`).Scan(&toA))
	assert.Equal(t, recipientsToJSON([]string{primary}), toA, "re-run leaves the backfilled companion intact")
}
