package db

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestFederationMoveAbandonWinsAgainstDelayedConfirmation(t *testing.T) {
	setupTestDB(t)
	m := FederationAgentMove{Direction: "out", Peer: "peer", ID: "offer", SourceAgent: "source", State: "awaiting_confirmation", SourceGroups: []int64{42}}
	require.NoError(t, InsertFederationAgentMove(m))
	stale := m
	m.State = "abandoned"
	won, err := TransitionFederationAgentMove(m, "awaiting_confirmation")
	require.NoError(t, err)
	require.True(t, won)
	stale.State = "confirmed"
	won, err = TransitionFederationAgentMove(stale, "awaiting_confirmation")
	require.NoError(t, err)
	require.False(t, won)
	got, err := GetFederationAgentMove("out", "peer", "offer")
	require.NoError(t, err)
	require.Equal(t, "abandoned", got.State)
	require.Equal(t, []int64{42}, got.SourceGroups)
	// A fresh move can start after abandon, but concurrent intents cannot.
	m.ID = "new-offer"
	m.State = "awaiting_confirmation"
	require.NoError(t, InsertFederationAgentMove(m))
	m.ID = "duplicate"
	require.Error(t, InsertFederationAgentMove(m))
}

func TestFederationMoveRetirementRejectsRotatedGenerationAtomically(t *testing.T) {
	setupTestDB(t)
	aid, _, err := EnsureAgentForConv("old-generation", "test")
	require.NoError(t, err)
	require.NoError(t, GrantAgentPermission("old-generation", "self.rename", "test"))
	_, err = RotateAgentConv("old-generation", "new-generation", "clear")
	require.NoError(t, err)
	// The stable actor is still resolved by the old handle, but this transaction
	// must refuse to revoke its successor's authority.
	_, err = RetireAgentAuthorizationAtGeneration("old-generation", "human", "move")
	require.ErrorContains(t, err, "generation changed")
	a, err := GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	require.Equal(t, "new-generation", a.CurrentConvID)
	permissions, err := ListAgentPermissionOverrideRowsForConv("new-generation")
	require.NoError(t, err)
	require.Len(t, permissions, 1)
	out, err := RetireAgentAuthorizationAtGeneration("new-generation", "human", "move")
	require.NoError(t, err)
	require.True(t, out.Retired)
	require.Equal(t, int64(1), out.PermsRevoked)
}
