package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// convRole reads agent_conversations.role for a conv (test helper).
func convRole(t *testing.T, conv string) string {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	var role string
	require.NoError(t, d.QueryRow(`SELECT role FROM agent_conversations WHERE conv_id = ?`, conv).Scan(&role))
	return role
}

// TestAdvanceAgentToNewConv_CASMissLinksGenerationNotHead pins the role
// ordering fix: when the CAS misses (oldConv is not the live head), the
// successor must be linked as a GENERATION, never a phantom second head, and
// the live pointer must not move.
func TestAdvanceAgentToNewConv_CASMissLinksGenerationNotHead(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("head0", "spawn")
	require.NoError(t, err)

	// Advance from a STALE old conv that isn't the current head → CAS misses.
	moved, err := advanceAgentToNewConv(d, agentID, "stale-old", "succ", "reincarnate", "", time.Now())
	require.NoError(t, err)
	assert.False(t, moved, "a stale-head advance must not move the pointer")

	assert.Equal(t, ConvRoleGeneration, convRole(t, "succ"),
		"a missed CAS must leave the successor a generation, not a second head")
	assert.Equal(t, ConvRoleHead, convRole(t, "head0"), "the real head keeps its role")
	a, _ := GetAgent(agentID)
	assert.Equal(t, "head0", a.CurrentConvID, "the live pointer is unchanged")
}

// TestAdvanceAgentToNewConv_PrelinkedGenerationPromoted: a successor already
// linked to the SAME actor as a generation is correctly promoted to head on a
// successful advance.
func TestAdvanceAgentToNewConv_PrelinkedGenerationPromoted(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	agentID, err := AllocateAgent("c0", "spawn")
	require.NoError(t, err)
	require.NoError(t, LinkConvToAgent("c1", agentID, ConvRoleGeneration, "prelink"))

	moved, err := advanceAgentToNewConv(d, agentID, "c0", "c1", "reincarnate", "worker", time.Now())
	require.NoError(t, err)
	assert.True(t, moved)

	assert.Equal(t, ConvRoleHead, convRole(t, "c1"), "the pre-linked successor is promoted to head")
	assert.Equal(t, ConvRoleGeneration, convRole(t, "c0"), "the old head is demoted")
	a, _ := GetAgent(agentID)
	assert.Equal(t, "c1", a.CurrentConvID)
	assert.Equal(t, "worker", a.PendingName, "the name is carried on a successful advance")
}

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

// TestLinkConvToAgentConflictAware: a conv belongs to exactly one actor —
// re-linking it to the SAME actor is an idempotent no-op, but re-linking it
// to a DIFFERENT actor is refused (error), never silently ignored.
func TestLinkConvToAgentConflictAware(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	a1, err := AllocateAgent("conv-x", "spawn")
	require.NoError(t, err)
	a2, err := AllocateAgent("conv-y", "spawn")
	require.NoError(t, err)

	// Re-link to the same actor — idempotent no-op.
	require.NoError(t, LinkConvToAgent("conv-x", a1, ConvRoleHead, "again"))

	// Try to steal conv-x onto a2 — must ERROR, and conv-x stays with a1.
	err = LinkConvToAgent("conv-x", a2, ConvRoleGeneration, "bogus")
	require.Error(t, err, "a cross-actor relink must be refused, not silently ignored")
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
	// The live pointer can only point at a linked generation of the actor.
	require.NoError(t, LinkConvToAgent("c1", agentID, ConvRoleGeneration, "test"))
	require.NoError(t, LinkConvToAgent("c2", agentID, ConvRoleGeneration, "test"))

	// A conv that is NOT linked to this actor is rejected outright.
	_, err = SetAgentCurrentConv(agentID, "c0", "stranger")
	require.Error(t, err, "the live pointer cannot point at an unlinked conv")

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

	active, err := ListActiveAgents()
	require.NoError(t, err)
	assert.Equal(t, []string{live}, agentIDs(active), "only the live actor is active")

	retired, err := ListRetiredAgents()
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
