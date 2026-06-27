package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerationsForAgent_ReturnsChainWithReasons verifies the richer twin of
// ConvsForAgent: every generation of an actor as a full row (conv_id, role,
// reason, linked_at), oldest link first, with the rotation reason preserved —
// what the dashboard's "Replaced generations" view annotates each predecessor
// with.
func TestGenerationsForAgent_ReturnsChainWithReasons(t *testing.T) {
	setupTestDB(t)

	// One actor, three generations: gen0 (spawn) → gen1 (clear) → gen2
	// (reincarnate), gen2 the live head.
	_, _, err := EnsureAgentForConv("gen0", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("gen0", "gen1", "clear")
	require.NoError(t, err, "rotate gen0→gen1")
	_, err = RotateAgentConv("gen1", "gen2", "reincarnate")
	require.NoError(t, err, "rotate gen1→gen2")

	actor, err := AgentIDForConv("gen2")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	gens, err := GenerationsForAgent(actor)
	require.NoError(t, err)
	require.Len(t, gens, 3, "all three generations returned")

	// Oldest link first, reasons preserved.
	assert.Equal(t, "gen0", gens[0].ConvID)
	assert.Equal(t, "gen1", gens[1].ConvID)
	assert.Equal(t, "gen2", gens[2].ConvID)
	assert.Equal(t, "clear", gens[1].Reason, "gen1 was linked via /clear")
	assert.Equal(t, "reincarnate", gens[2].Reason, "gen2 was linked via reincarnate")
	for _, g := range gens {
		assert.Equal(t, actor, g.AgentID, "each row carries the owning actor")
		assert.False(t, g.LinkedAt.IsZero(), "linked_at is parsed")
	}

	// Empty / unknown agent → no rows, no error.
	none, err := GenerationsForAgent("")
	require.NoError(t, err)
	assert.Empty(t, none)
	none, err = GenerationsForAgent("agent-does-not-exist")
	require.NoError(t, err)
	assert.Empty(t, none)
}
