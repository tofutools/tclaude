package db

import (
	"testing"
	"time"

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

	carriedName, err := RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err)
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

// TestRotateAgentConv_AbsorbsBareSuccessorActor reproduces the reincarnate
// ordering hazard: the successor's session-start hook self-registers its OWN
// (bare) actor for newConv before the rotation runs. RotateAgentConv must absorb
// that bare actor so identity still carries onto the predecessor's actor — the
// case the simSpawner can't reproduce (it writes the SessionRow without the
// session-start enroll), so it is pinned here at the db boundary.
func TestRotateAgentConv_AbsorbsBareSuccessorActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	gid, err := CreateAgentGroup("rot-grp", "")
	require.NoError(t, err)
	// The predecessor actor, with real identity (a group membership).
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: gid, ConvID: "old", Role: "lead"}))
	oldAgent, _ := AgentIDForConv("old")
	require.NotEmpty(t, oldAgent)

	// The successor self-registers its OWN bare actor (as the production
	// session-start hook would), BEFORE the rotation.
	newAgent, created, err := EnsureAgentForConv("new", "session-start")
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, oldAgent, newAgent, "successor starts on a separate actor")

	_, err = RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err, "rotation absorbs the bare successor and advances")

	// newConv now belongs to the PREDECESSOR's actor; the bare actor is gone.
	resolved, _ := AgentIDForConv("new")
	assert.Equal(t, oldAgent, resolved, "newConv was relinked onto the predecessor's actor")
	gone, _ := GetAgent(newAgent)
	assert.Nil(t, gone, "the bare self-registered actor was absorbed (deleted)")
	a, _ := GetAgent(oldAgent)
	require.NotNil(t, a)
	assert.Equal(t, "new", a.CurrentConvID, "the live pointer advanced to the successor")

	// Identity carried: the membership resolves from the new conv.
	groups, err := ListGroupsForConv("new")
	require.NoError(t, err)
	require.Len(t, groups, 1, "the predecessor's group membership carried onto the successor")
	assert.Equal(t, "rot-grp", groups[0].Name)
}

// TestRotateAgentConv_RefusesNonBareSuccessor: when the successor's conv is
// already owned by an actor that holds real identity (NOT a bare
// self-registration), the rotation must NOT destroy it — it fails (nothing
// committed) so the lifecycle caller aborts / retries rather than silently
// stranding the actor head.
func TestRotateAgentConv_RefusesNonBareSuccessor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	gid, err := CreateAgentGroup("rot-grp2", "")
	require.NoError(t, err)
	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err)
	oldAgent, _ := AgentIDForConv("old")

	// The successor's conv already belongs to a DIFFERENT actor with real
	// identity (a membership) — not absorbable.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: gid, ConvID: "new", Role: "lead"}))
	newAgent, _ := AgentIDForConv("new")
	require.NotEqual(t, oldAgent, newAgent)

	_, err = RotateAgentConv("old", "new", "reincarnate")
	require.Error(t, err, "a non-bare successor actor cannot be absorbed → rotation fails")

	// Nothing committed: both actors survive, pointer unchanged, no edge.
	a, _ := GetAgent(oldAgent)
	require.NotNil(t, a)
	assert.Equal(t, "old", a.CurrentConvID, "the predecessor actor head did not move")
	stillNew, _ := AgentIDForConv("new")
	assert.Equal(t, newAgent, stillNew, "the successor's actor is untouched")
	succ, err := GetConvSuccessor("old")
	require.NoError(t, err)
	assert.Empty(t, succ, "the succession edge rolled back with the failed rotation")
}

// TestRotateAgentConv_RefusesSuccessorWithSpawnHistory: the agent-keyed
// spawn/clone rate-limit history is actor-scoped state with no FK to agents, so
// a successor actor that has spawned (or was cloned-from) has "mattered" and
// must NOT be absorbed — otherwise the history rows would be silently orphaned.
func TestRotateAgentConv_RefusesSuccessorWithSpawnHistory(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err)
	oldAgent, _ := AgentIDForConv("old")

	// The successor self-registers a fresh actor and then records a spawn —
	// actor-scoped history keyed on its agent_id.
	newAgent, _, err := EnsureAgentForConv("new", "session-start")
	require.NoError(t, err)
	require.NoError(t, ClaimSpawnSlot("new", 10, time.Hour, time.Now()), "record spawn history")

	_, err = RotateAgentConv("old", "new", "reincarnate")
	require.Error(t, err, "a successor with spawn history is not bare → rotation fails, not a silent orphan")

	stillNew, _ := GetAgent(newAgent)
	assert.NotNil(t, stillNew, "the successor actor (with history) is not absorbed")
	a, _ := GetAgent(oldAgent)
	assert.Equal(t, "old", a.CurrentConvID, "the predecessor actor head did not move")
}
