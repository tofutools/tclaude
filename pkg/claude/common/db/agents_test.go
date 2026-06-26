package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllocateAndResolveAgent covers the core actor-identity round trip:
// minting an agent for a conv and resolving back to it from every angle.
func TestAllocateAndResolveAgent(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("conv-a", "spawn")
	require.NoError(t, err, "AllocateAgent")
	assert.True(t, len(agentID) > len(agentIDPrefix) && agentID[:len(agentIDPrefix)] == agentIDPrefix,
		"agent_id should carry the agt_ prefix, got %q", agentID)

	got, err := AgentIDForConv("conv-a")
	require.NoError(t, err, "AgentIDForConv")
	assert.Equal(t, agentID, got, "conv resolves to its actor")

	a, err := GetAgent(agentID)
	require.NoError(t, err, "GetAgent")
	require.NotNil(t, a)
	assert.Equal(t, "conv-a", a.CurrentConvID)
	assert.Equal(t, "spawn", a.CreatedVia)
	assert.True(t, a.Active(), "a fresh agent is active")

	byConv, err := GetAgentByConv("conv-a")
	require.NoError(t, err, "GetAgentByConv")
	require.NotNil(t, byConv)
	assert.Equal(t, agentID, byConv.AgentID)

	convs, err := ConvsForAgent(agentID)
	require.NoError(t, err, "ConvsForAgent")
	assert.Equal(t, []string{"conv-a"}, convs)
}

// TestAgentIDForConvUnknown: an unmapped conv resolves to "".
func TestAgentIDForConvUnknown(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	got, err := AgentIDForConv("nope")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	a, err := GetAgentByConv("nope")
	require.NoError(t, err)
	assert.Nil(t, a)
}

// TestAllocateAgentRejectsDuplicate: AllocateAgent is strict — a second
// allocation for the same conv is an error (callers wanting idempotency use
// EnsureAgentForConv).
func TestAllocateAgentRejectsDuplicate(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	_, err = AllocateAgent("conv-dup", "spawn")
	require.NoError(t, err)
	_, err = AllocateAgent("conv-dup", "spawn")
	assert.Error(t, err, "second allocate for the same conv must fail")
}

// TestEnsureAgentForConvIdempotent: first call mints, repeat returns the same
// actor with created=false.
func TestEnsureAgentForConvIdempotent(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	id1, created1, err := EnsureAgentForConv("conv-e", "cli")
	require.NoError(t, err)
	assert.True(t, created1, "first ensure mints an actor")

	id2, created2, err := EnsureAgentForConv("conv-e", "cli")
	require.NoError(t, err)
	assert.False(t, created2, "second ensure is a no-op")
	assert.Equal(t, id1, id2, "same conv keeps the same actor")
}

// TestLinkConvToAgent models a rotation: a second conv generation joins an
// existing actor and resolves to it; the actor now owns both generations.
func TestLinkConvToAgent(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("gen-1", "spawn")
	require.NoError(t, err)

	require.NoError(t, LinkConvToAgent("gen-2", agentID, ConvRoleGeneration, "reincarnate"))

	got, err := AgentIDForConv("gen-2")
	require.NoError(t, err)
	assert.Equal(t, agentID, got, "the new generation resolves to the same actor")

	convs, err := ConvsForAgent(agentID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"gen-1", "gen-2"}, convs)
}

// TestLinkConvToAgentIsIdempotent: a conv belongs to exactly one actor; a
// re-link of the same conv is a no-op and never reassigns it.
func TestLinkConvToAgentIsIdempotent(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	a1, err := AllocateAgent("conv-x", "spawn")
	require.NoError(t, err)
	a2, err := AllocateAgent("conv-y", "spawn")
	require.NoError(t, err)

	// Try to steal conv-x onto a2 — must be ignored (conv_id PK).
	require.NoError(t, LinkConvToAgent("conv-x", a2, ConvRoleGeneration, "bogus"))
	got, err := AgentIDForConv("conv-x")
	require.NoError(t, err)
	assert.Equal(t, a1, got, "an existing conv link is never reassigned")
}

// TestSetAgentCurrentConvCAS pins the compare-and-swap semantics that protect
// against two racing rotations advancing the same actor from stale state.
func TestSetAgentCurrentConvCAS(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("c0", "spawn")
	require.NoError(t, err)

	// Stale expectation → no move.
	moved, err := SetAgentCurrentConv(agentID, "wrong-old", "c1")
	require.NoError(t, err)
	assert.False(t, moved, "CAS must not move when the expected old conv mismatches")
	a, _ := GetAgent(agentID)
	assert.Equal(t, "c0", a.CurrentConvID)

	// Correct expectation → moves.
	moved, err = SetAgentCurrentConv(agentID, "c0", "c1")
	require.NoError(t, err)
	assert.True(t, moved, "CAS moves when the expected old conv matches")
	a, _ = GetAgent(agentID)
	assert.Equal(t, "c1", a.CurrentConvID)

	// Unconditional set (expected == "").
	moved, err = SetAgentCurrentConv(agentID, "", "c2")
	require.NoError(t, err)
	assert.True(t, moved)
	a, _ = GetAgent(agentID)
	assert.Equal(t, "c2", a.CurrentConvID)
}

// TestRetireReinstateAndRoster covers actor-level retire/reinstate and the
// agent-keyed rosters.
func TestRetireReinstateAndRoster(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	live, err := AllocateAgent("live-conv", "spawn")
	require.NoError(t, err)
	gone, err := AllocateAgent("gone-conv", "spawn")
	require.NoError(t, err)

	ok, err := RetireAgentByID(gone, "human", "cleanup")
	require.NoError(t, err)
	assert.True(t, ok, "retiring an active agent returns true")

	// Idempotent: retiring again is a no-op.
	ok, err = RetireAgentByID(gone, "human", "cleanup")
	require.NoError(t, err)
	assert.False(t, ok)

	active, err := ListActiveAgents2()
	require.NoError(t, err)
	assert.Equal(t, []string{live}, agentIDs(active), "only the live actor is active")

	retired, err := ListRetiredAgents2()
	require.NoError(t, err)
	assert.Equal(t, []string{gone}, agentIDs(retired))

	a, _ := GetAgent(gone)
	assert.False(t, a.Active())
	assert.Equal(t, "human", a.RetiredBy)
	assert.Equal(t, "cleanup", a.RetireReason)

	// Reinstate restores it.
	ok, err = ReinstateAgentByID(gone)
	require.NoError(t, err)
	assert.True(t, ok)
	a, _ = GetAgent(gone)
	assert.True(t, a.Active())
}

// TestSetAgentPendingName: the actor-level display-name fallback.
func TestSetAgentPendingName(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("named-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, SetAgentPendingName(agentID, "worker-7"))

	a, _ := GetAgent(agentID)
	assert.Equal(t, "worker-7", a.PendingName)
}

func agentIDs(agents []*Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.AgentID)
	}
	return out
}
