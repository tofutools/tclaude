package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeAutoPermitCondition(t *testing.T) {
	ok, err := NormalizeAutoPermitCondition("  enter-worktree  ")
	require.NoError(t, err)
	assert.Equal(t, "enter-worktree", ok, "trims surrounding whitespace")

	_, err = NormalizeAutoPermitCondition("   ")
	assert.Error(t, err, "empty-after-trim rejected")

	_, err = NormalizeAutoPermitCondition("Enter-Worktree")
	assert.Error(t, err, "upper case rejected — a stored name compares byte-for-byte with the registry")

	_, err = NormalizeAutoPermitCondition("enter worktree")
	assert.Error(t, err, "space rejected")

	_, err = NormalizeAutoPermitCondition(strings.Repeat("x", MaxAutoPermitConditionLen+1))
	assert.Error(t, err, "over-length rejected")
}

func TestAgentAutoPermit_SetListClear(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_a', 'conv-a', 1783123200000000000)`)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_b', 'conv-b', 1783123200000000000)`)

	now := time.Now()
	require.NoError(t, SetAgentAutoPermit("agt_a", "enter-worktree", "human", now))

	got, err := ListAgentAutoPermits("agt_a")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "enter-worktree", got[0].Condition)
	assert.Equal(t, "human", got[0].GrantedBy)
	assert.WithinDuration(t, now, got[0].CreatedAt, time.Second)

	// Re-consenting refreshes the row rather than duplicating it, and the row
	// names the most recent granter.
	later := now.Add(time.Hour)
	require.NoError(t, SetAgentAutoPermit("agt_a", "enter-worktree", "po-agent", later))
	got, err = ListAgentAutoPermits("agt_a")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "po-agent", got[0].GrantedBy)
	assert.WithinDuration(t, later, got[0].CreatedAt, time.Second)

	// An agent with no opt-ins reads as empty, not as an error.
	none, err := ListAgentAutoPermits("agt_b")
	require.NoError(t, err)
	assert.Empty(t, none)

	// The sweep's one-query view is grouped by agent.
	require.NoError(t, SetAgentAutoPermit("agt_b", "enter-worktree", "human", now))
	all, err := ListAllAutoPermits()
	require.NoError(t, err)
	assert.True(t, all["agt_a"]["enter-worktree"])
	assert.True(t, all["agt_b"]["enter-worktree"])

	// Clear reports whether anything was actually revoked.
	removed, err := ClearAgentAutoPermit("agt_a", "enter-worktree")
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = ClearAgentAutoPermit("agt_a", "enter-worktree")
	require.NoError(t, err)
	assert.False(t, removed, "revoking what was never on is not an error, just a no-op")

	got, err = ListAgentAutoPermits("agt_a")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A stale opt-in whose condition no build registers must stay revocable by
// name: the store is deliberately agnostic about the registry's vocabulary.
func TestAgentAutoPermit_UnknownConditionStaysRevocable(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_a', 'conv-a', 1783123200000000000)`)

	require.NoError(t, SetAgentAutoPermit("agt_a", "retired-condition", "human", time.Now()))
	removed, err := ClearAgentAutoPermit("agt_a", "retired-condition")
	require.NoError(t, err)
	assert.True(t, removed)
}

func TestAgentAutoPermit_Validation(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_a', 'conv-a', 1783123200000000000)`)

	assert.Error(t, SetAgentAutoPermit("", "enter-worktree", "human", time.Now()),
		"agent_id is required")
	assert.Error(t, SetAgentAutoPermit("agt_a", "Enter Worktree", "human", time.Now()),
		"a malformed condition name is rejected at the store boundary")
}
