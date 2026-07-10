package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCancelAgentMessageNudge_RevalidatesTargetAtomically pins the guard that
// closes the sweep/reinstate race: the reaper sweep decides "target retired"
// from a (possibly cached) read, so by the time it stamps the cancellation the
// agent may have been reinstated — and reinstate only clears cancellations
// that exist inside ITS transaction, so a late stamp would strand the message
// forever. The UPDATE therefore re-checks the agents row itself: it must
// refuse to cancel when the target is active, and cancel when it is retired
// or its actor row is gone entirely.
func TestCancelAgentMessageNudge_RevalidatesTargetAtomically(t *testing.T) {
	setupTestDB(t)

	g, _ := CreateAgentGroup("alpha", "")
	const conv = "cancel-guard-conv-1"
	agentID, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)

	id, err := InsertAgentMessage(&AgentMessage{
		GroupID: g, FromConv: "conv-x", ToConv: conv, Body: "queued",
	})
	require.NoError(t, err)

	// Active target: the stale-verdict stamp must be refused.
	cancelled, err := CancelAgentMessageNudge(id, agentID, time.Now(), "target agent retired")
	require.NoError(t, err)
	assert.False(t, cancelled, "an ACTIVE target must not be cancelled (stale sweep verdict)")
	m, err := GetAgentMessage(id)
	require.NoError(t, err)
	assert.True(t, m.NudgeCancelledAt.IsZero())

	// Retired target: cancels.
	retired, err := RetireAgentByID(agentID, "human", "test")
	require.NoError(t, err)
	require.True(t, retired)
	cancelled, err = CancelAgentMessageNudge(id, agentID, time.Now(), "target agent retired")
	require.NoError(t, err)
	assert.True(t, cancelled, "retired target cancels")

	// Idempotent: a second stamp reports false so the caller logs once.
	cancelled, err = CancelAgentMessageNudge(id, agentID, time.Now(), "target agent retired")
	require.NoError(t, err)
	assert.False(t, cancelled, "already-cancelled row is not re-cancelled")

	// Reinstate revives the queue...
	reinstated, err := ReinstateAgentByID(agentID)
	require.NoError(t, err)
	require.True(t, reinstated)
	m, err = GetAgentMessage(id)
	require.NoError(t, err)
	assert.True(t, m.NudgeCancelledAt.IsZero(), "reinstate clears the cancellation")
	assert.Empty(t, m.NudgeCancelReason)

	// ...and a deleted target (no agents row at all) also counts as gone.
	cancelled, err = CancelAgentMessageNudge(id, "agt_does_not_exist", time.Now(), "target agent deleted")
	require.NoError(t, err)
	assert.True(t, cancelled, "missing actor row cancels")
}
