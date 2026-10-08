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
