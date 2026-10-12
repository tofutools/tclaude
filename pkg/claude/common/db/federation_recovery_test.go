package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recoveryLocal = "inst_recoverylocal000000000000"

func TestRecoveredIdentityNewAndConflict(t *testing.T) {
	setupTestDB(t)
	fresh := NewAgentID()
	require.NoError(t, ReserveRecoveredIdentity(fresh, recoveryLocal, "recover-a", false))
	require.Error(t, ReserveRecoveredIdentity(fresh, recoveryLocal, "recover-b", true), "a launch in progress is never replaced")
	require.NoError(t, ReleaseFederationIdentity(fresh, "recover-a"))
	p, err := GetAgentFederationPresence(fresh)
	require.NoError(t, err)
	assert.Nil(t, p, "an undispatched reservation for a new id leaves nothing behind")

	existing, _, err := EnsureAgentForConv("conv-recover-existing", "spawn")
	require.NoError(t, err)
	require.Error(t, ReserveRecoveredIdentity(existing, recoveryLocal, "recover-c", false), "an existing agent is a conflict without force")
}

// The pending-spawn claim binds a forced recovery over an existing agent:
// the launch becomes the agent's head generation.
func TestRecoveredIdentityBindsOnPendingClaim(t *testing.T) {
	setupTestDB(t)
	id, _, err := EnsureAgentForConv("conv-recover-old", "spawn")
	require.NoError(t, err)
	require.NoError(t, ReserveRecoveredIdentity(id, recoveryLocal, "recover-p", true))
	require.NoError(t, InsertPendingSpawn(&PendingSpawn{Label: "spwn-recover", AgentID: id, GroupID: 1}))

	claimed, err := ClaimPendingSpawnAndBindAgent("spwn-recover", "conv-recover-new", id, "spawn")
	require.NoError(t, err)
	assert.True(t, claimed)
	a, err := GetAgent(id)
	require.NoError(t, err)
	assert.Equal(t, "conv-recover-new", a.CurrentConvID)
	p, err := GetAgentFederationPresence(id)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "here", p.State)
	assert.Equal(t, recoveryLocal, p.HomeInstance)
}

// A forced recovery that fails after binding restores the agent it took
// over: its head, its retirement state and its grants.
func TestRecoveredIdentityRollbackRestoresTakenOverAgent(t *testing.T) {
	setupTestDB(t)
	id, _, err := EnsureAgentForConv("conv-rollback-old", "spawn")
	require.NoError(t, err)
	require.NoError(t, GrantAgentPermission("conv-rollback-old", "agent.spawn", "human"))
	_, err = RetireAgentByID(id, "human", "parked")
	require.NoError(t, err)

	require.NoError(t, ReserveRecoveredIdentity(id, recoveryLocal, "recover-r", true))
	got, _, err := EnsureAgentForConvWithID("conv-rollback-new", id, "spawn")
	require.NoError(t, err)
	require.Equal(t, id, got)

	rolled, err := RollbackFederationArrival("conv-rollback-new")
	require.NoError(t, err)
	assert.True(t, rolled)
	a, err := GetAgent(id)
	require.NoError(t, err)
	assert.Equal(t, "conv-rollback-old", a.CurrentConvID)
	assert.False(t, a.Active())
	assert.Equal(t, "parked", a.RetireReason)
	has, err := HasAgentPermissionRow("conv-rollback-old", "agent.spawn")
	require.NoError(t, err)
	assert.True(t, has, "the taken-over agent keeps its grants")
	p, err := GetAgentFederationPresence(id)
	require.NoError(t, err)
	assert.Nil(t, p, "an agent that had no presence gets none")
}
