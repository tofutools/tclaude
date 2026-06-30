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

// TestGenerationsForAgent_OrdersByLinkNotLexicalTimestamp pins the ordering
// against a regression to a lexicographic linked_at sort. linked_at is stored
// as RFC3339Nano, whose variable-width fractional seconds do NOT string-sort
// chronologically: a generation that lands exactly on a whole second formats
// with no fraction ("…43Z"), and '.' (0x2E) < 'Z' (0x5A), so a same-second
// generation that DOES carry a fraction ("…43.0000018Z") sorts BEFORE it. With
// `ORDER BY linked_at` the oldest generation would come back last — the live-CI
// flake that bit this suite when gen0 happened to fall on a whole second. The
// readers must order by rowid (link-insertion order) instead, so this passes
// deterministically every run rather than once in a blue moon.
func TestGenerationsForAgent_OrdersByLinkNotLexicalTimestamp(t *testing.T) {
	setupTestDB(t)

	// Build a 3-generation chain through the normal path so the rows are
	// inserted in link order (g0 → g1 → g2) and get ascending rowids.
	_, _, err := EnsureAgentForConv("g0", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, err = RotateAgentConv("g0", "g1", "clear")
	require.NoError(t, err, "rotate g0→g1")
	_, err = RotateAgentConv("g1", "g2", "reincarnate")
	require.NoError(t, err, "rotate g1→g2")
	actor, err := AgentIDForConv("g2")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// Stamp the exact pathological timestamps: the OLDEST link (g0) on a whole
	// second (no fraction), the later two with fractions that string-sort ahead
	// of it. A lexicographic linked_at sort would yield g1, g2, g0.
	d, err := Open()
	require.NoError(t, err)
	for conv, at := range map[string]string{
		"g0": "2026-06-30T20:13:43Z",
		"g1": "2026-06-30T20:13:43.0000018Z",
		"g2": "2026-06-30T20:13:43.0000025Z",
	} {
		_, err = d.Exec(`UPDATE agent_conversations SET linked_at = ? WHERE conv_id = ?`, at, conv)
		require.NoError(t, err, "stamp linked_at for %s", conv)
	}

	gens, err := GenerationsForAgent(actor)
	require.NoError(t, err)
	require.Len(t, gens, 3)
	assert.Equal(t, []string{"g0", "g1", "g2"},
		[]string{gens[0].ConvID, gens[1].ConvID, gens[2].ConvID},
		"generations must return in link/rowid order, not lexicographic linked_at order")

	// ConvsForAgent shares the ordering; pin its lighter twin too.
	convs, err := ConvsForAgent(actor)
	require.NoError(t, err)
	assert.Equal(t, []string{"g0", "g1", "g2"}, convs)
}
