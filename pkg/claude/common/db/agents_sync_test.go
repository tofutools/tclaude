package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the single-source actor lifecycle: since JOH-26 PR3c the
// agents table is the only roster, so the conv-keyed helpers (EnsureAgentForConv
// / PromoteAgent / RetireAgent / ReinstateAgent) must mint / flip the actor row
// directly when callers go through the DB helpers, not only through the
// spawn/clear daemon flows. Without this, a grant / group-add / promote / retire
// would leave the agents table inconsistent (e.g. the agent-keyed roster would
// resurrect a retired agent).

// TestEnsureAgentForConvMintsActor: the catch-all ensure mints the stable actor.
func TestEnsureAgentForConvMintsActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	agentID, created, err := EnsureAgentForConv("ec1", "group")
	require.NoError(t, err)
	require.True(t, created, "first ensure mints a new actor")
	require.NotEmpty(t, agentID, "EnsureAgentForConv must mint an actor for the conv")

	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.Equal(t, "group", a.CreatedVia)
	assert.True(t, a.Active())

	// Idempotent: a second ensure does not mint a second actor.
	again, created2, err := EnsureAgentForConv("ec1", "cli")
	require.NoError(t, err)
	assert.False(t, created2, "a second ensure reuses the actor")
	assert.Equal(t, agentID, again)
}

// TestAddGroupMemberEnsuresActor: a higher-level helper that funnels through
// EnsureAgentForConv (group-add) also gets the actor for free.
func TestAddGroupMemberEnsuresActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	gid, err := CreateAgentGroup("sync-grp", "")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: gid, ConvID: "mem1"}))

	agentID, err := AgentIDForConv("mem1")
	require.NoError(t, err)
	assert.NotEmpty(t, agentID, "joining a group must mint the actor too")
}

// TestRetireAgentRetiresActor: a human retire of a live conv demotes the
// mapped actor so the agent-keyed roster drops it.
func TestRetireAgentRetiresActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	_, _, err = EnsureAgentForConv("rc1", "cli")
	require.NoError(t, err)
	agentID, _ := AgentIDForConv("rc1")

	ok, err := RetireAgent("rc1", "human", "cleanup")
	require.NoError(t, err)
	require.True(t, ok)

	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.False(t, a.Active(), "the actor must be retired")
	assert.Equal(t, "human", a.RetiredBy)

	active, _ := ListActiveAgents()
	assert.NotContains(t, agentIDs(active), agentID, "a retired actor is off the active roster")
}

// TestReinstateAgentReinstatesActor: reinstate brings the actor back.
func TestReinstateAgentReinstatesActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	_, _, err = EnsureAgentForConv("rc2", "cli")
	require.NoError(t, err)
	agentID, _ := AgentIDForConv("rc2")
	_, err = RetireAgent("rc2", "human", "cleanup")
	require.NoError(t, err)

	ok, err := ReinstateAgent("rc2")
	require.NoError(t, err)
	require.True(t, ok)

	a, _ := GetAgent(agentID)
	assert.True(t, a.Active(), "reinstate must clear the actor's retire")
}

// TestPromoteAgentActivatesActor: promoting a plain conv mints an active
// actor; promoting (reinstating) a retired conv reactivates its actor.
func TestPromoteAgentActivatesActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	// none → active
	prior, err := PromoteAgent("pc1", "promote")
	require.NoError(t, err)
	assert.Equal(t, AgentStateNone, prior)
	agentID, _ := AgentIDForConv("pc1")
	require.NotEmpty(t, agentID)
	a, _ := GetAgent(agentID)
	assert.True(t, a.Active())

	// retire then promote (reinstate path) → active again
	_, err = RetireAgent("pc1", "human", "x")
	require.NoError(t, err)
	prior, err = PromoteAgent("pc1", "promote")
	require.NoError(t, err)
	assert.Equal(t, AgentStateRetired, prior)
	a, _ = GetAgent(agentID)
	assert.True(t, a.Active(), "promoting a retired conv reactivates its actor")
}

// TestRotateAgentConv_PreservesActorAndCarriesName drives the rotation core
// directly: it must keep the SAME actor across old→new, advance the live
// pointer, and carry the display name onto the ACTOR row. The predecessor stays
// a generation of the still-active actor — it is NOT retired (the actor-level
// model PR3b/PR3c shipped: a predecessor is a past generation, not a standalone
// retired entry).
func TestRotateAgentConv_PreservesActorAndCarriesName(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	oldAgent, _, err := EnsureAgentForConv("old", "spawn")
	require.NoError(t, err)
	require.NotEmpty(t, oldAgent)
	require.NoError(t, SetAgentPendingName(oldAgent, "carried-worker"))

	carriedName, moved, err := RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err)
	assert.True(t, moved, "the live pointer advanced")
	assert.Equal(t, "carried-worker", carriedName)

	newAgent, _ := AgentIDForConv("new")
	assert.Equal(t, oldAgent, newAgent, "the actor is preserved across the rotation")

	a, _ := GetAgent(newAgent)
	require.NotNil(t, a)
	assert.Equal(t, "new", a.CurrentConvID, "the live pointer advanced to the successor")
	assert.True(t, a.Active(), "a rotation never retires the actor")
	assert.Equal(t, "carried-worker", a.PendingName,
		"the display name is carried onto the actor row")

	// The predecessor is a past generation of the still-active actor — both
	// generations resolve to the same live agent, neither is retired.
	st, _ := AgentState("old")
	assert.Equal(t, AgentStateActive, st, "the predecessor generation resolves to the active actor")
	stNew, _ := AgentState("new")
	assert.Equal(t, AgentStateActive, stNew, "the successor is the live generation of the active actor")
}
