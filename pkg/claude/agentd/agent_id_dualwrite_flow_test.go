package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These flow tests cover the stable agent-identity dual-write (JOH-26, PR1):
// the lifecycle paths populate the new `agents` / `agent_conversations`
// tables through the production daemon, with NO change to existing
// behaviour. They assert the new actor layer at the DB surface (db.* reads),
// which is where the cutover stage will later route authorization.

// Scenario: `tclaude agent spawn alpha --name <n>` mints a brand-new actor.
//
// Expected: the spawned conv resolves to a fresh agent_id, the actor's live
// pointer is that conv, and the actor carries the spawn-time name and via.
func TestSpawn_AllocatesStableAgentID(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "stable-worker")

	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "a spawned conv must get an agent_id")

	a, err := db.GetAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, spawn.ConvID, a.CurrentConvID, "the actor's live conv is the spawned conv")
	assert.Equal(t, "spawn", a.CreatedVia)
	assert.Equal(t, "stable-worker", a.PendingName,
		"the spawn-time name lands on the actor row too")
	assert.True(t, a.Active())
}

// Scenario: a provisioned agent is /clear'd (CC rotates its conv-id).
//
// Expected: the actor identity is PRESERVED across the rotation — the new
// conv resolves to the SAME agent_id as the old, the live pointer advances
// to the new conv, and the old generation is demoted from head. This is the
// dual-write that the cutover stage relies on to drop the physical rekey.
func TestClearRotation_PreservesStableAgentID(t *testing.T) {
	f := newFlow(t)
	setupClearedAgent(t, f)

	c := f.Clear(clearAgentLabel)
	require.NotEqual(t, c.OldConv, c.NewConv, "conv-id must rotate on /clear")

	oldAgent, err := db.AgentIDForConv(c.OldConv)
	require.NoError(t, err)
	newAgent, err := db.AgentIDForConv(c.NewConv)
	require.NoError(t, err)
	require.NotEmpty(t, newAgent, "the post-/clear conv must belong to an actor")
	assert.Equal(t, oldAgent, newAgent,
		"the actor identity is preserved across /clear — both generations are one actor")

	a, err := db.GetAgent(newAgent)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, c.NewConv, a.CurrentConvID, "the live pointer advanced to the new conv")
	assert.True(t, a.Active(), "the actor stays active — a rotation is not a retirement")

	// Both generations are linked to the actor; the new one is head.
	convs, err := db.ConvsForAgent(newAgent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c.OldConv, c.NewConv}, convs,
		"both conversation generations belong to the actor")

	newRole, oldRole := convRoleFor(t, c.NewConv), convRoleFor(t, c.OldConv)
	assert.Equal(t, db.ConvRoleHead, newRole, "the new generation is the head")
	assert.Equal(t, db.ConvRoleGeneration, oldRole, "the old generation is demoted from head")
}

// convRoleFor reads agent_conversations.role for a conv — a thin DB probe so
// the test can assert the head/generation demotion the rotation performs.
func convRoleFor(t *testing.T, conv string) string {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	var role string
	require.NoError(t, d.QueryRow(
		`SELECT role FROM agent_conversations WHERE conv_id = ?`, conv).Scan(&role))
	return role
}
