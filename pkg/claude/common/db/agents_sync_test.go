package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the centralized dual-write: the actor layer must stay in
// lockstep with enrollment when callers go through the DB helpers DIRECTLY
// (EnrollAgent / PromoteAgent / RetireAgent / ReinstateAgent), not only
// through the spawn/clear daemon flows. Without this, a post-v72 grant /
// group-add / promote / retire would drift the agents table (e.g. the
// agent-keyed roster would resurrect a retired agent).

// TestEnrollAgentEnsuresActor: EnrollAgent now also ensures the stable actor.
func TestEnrollAgentEnsuresActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	require.NoError(t, EnrollAgent("ec1", "group"))
	agentID, err := AgentIDForConv("ec1")
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "EnrollAgent must mint an actor for the conv")

	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.Equal(t, "group", a.CreatedVia)
	assert.True(t, a.Active())

	// Idempotent: a second enroll does not mint a second actor.
	require.NoError(t, EnrollAgent("ec1", "cli"))
	again, _ := AgentIDForConv("ec1")
	assert.Equal(t, agentID, again)
}

// TestAddGroupMemberEnsuresActor: a higher-level helper that funnels through
// EnrollAgent (group-add) also gets the actor for free.
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

	require.NoError(t, EnrollAgent("rc1", "cli"))
	agentID, _ := AgentIDForConv("rc1")

	ok, err := RetireAgent("rc1", "human", "cleanup")
	require.NoError(t, err)
	require.True(t, ok)

	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.False(t, a.Active(), "the actor must be retired alongside the enrollment")
	assert.Equal(t, "human", a.RetiredBy)

	active, _ := ListActiveAgents2()
	assert.NotContains(t, agentIDs(active), agentID, "a retired actor is off the active roster")
}

// TestReinstateAgentReinstatesActor: reinstate brings the actor back.
func TestReinstateAgentReinstatesActor(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	require.NoError(t, EnrollAgent("rc2", "cli"))
	agentID, _ := AgentIDForConv("rc2")
	_, err = RetireAgent("rc2", "human", "cleanup")
	require.NoError(t, err)

	ok, err := ReinstateAgent("rc2")
	require.NoError(t, err)
	require.True(t, ok)

	a, _ := GetAgent(agentID)
	assert.True(t, a.Active(), "reinstate must clear the actor's retire too")
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
	assert.Equal(t, EnrollmentNone, prior)
	agentID, _ := AgentIDForConv("pc1")
	require.NotEmpty(t, agentID)
	a, _ := GetAgent(agentID)
	assert.True(t, a.Active())

	// retire then promote (reinstate path) → active again
	_, err = RetireAgent("pc1", "human", "x")
	require.NoError(t, err)
	prior, err = PromoteAgent("pc1", "promote")
	require.NoError(t, err)
	assert.Equal(t, EnrollmentRetired, prior)
	a, _ = GetAgent(agentID)
	assert.True(t, a.Active(), "promoting a retired conv reactivates its actor")
}

// TestMigrateAgentIdentity_PreservesActorAndCarriesName drives the rotation
// core directly: it must keep the SAME actor across old→new, advance the live
// pointer, and carry the display name onto the ACTOR row (not just the new
// conv's enrollment). The predecessor's enrollment is retired (legacy
// behaviour) but that must NOT retire the actor — the actor lives on at new.
func TestMigrateAgentIdentity_PreservesActorAndCarriesName(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	require.NoError(t, EnrollAgent("old", "spawn"))
	require.NoError(t, SetEnrollmentPendingName("old", "carried-worker"))
	oldAgent, _ := AgentIDForConv("old")
	require.NotEmpty(t, oldAgent)

	_, err = MigrateAgentIdentity("old", "new", "reincarnate", "system:test")
	require.NoError(t, err)

	newAgent, _ := AgentIDForConv("new")
	assert.Equal(t, oldAgent, newAgent, "the actor is preserved across the rotation")

	a, _ := GetAgent(newAgent)
	require.NotNil(t, a)
	assert.Equal(t, "new", a.CurrentConvID, "the live pointer advanced to the successor")
	assert.True(t, a.Active(), "a rotation never retires the actor")
	assert.Equal(t, "carried-worker", a.PendingName,
		"the display name is carried onto the actor row, not just the new enrollment")

	// The predecessor enrollment retires (legacy), but the actor does not.
	st, _ := EnrollmentState("old")
	assert.Equal(t, EnrollmentRetired, st, "predecessor enrollment retires (legacy)")
}
