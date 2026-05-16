package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClaimSpawnSlot is the atomic count-and-insert behind the per-caller
// spawn rate limit (the third spawn guardrail). These exercise it in
// isolation — the end-to-end 429 is covered by the flow tests.

func TestClaimSpawnSlot_RejectsEmptySpawner(t *testing.T) {
	setupTestDB(t)
	require.Error(t, ClaimSpawnSlot("", 10, time.Hour, time.Now()),
		"an empty spawnerConvID must be rejected")
}

func TestClaimSpawnSlot_UnlimitedWhenMaxNonPositive(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	// maxPerWindow <= 0 disables the rate limit: every claim passes and
	// — the cheap-path guarantee — nothing is recorded at all.
	for i := 0; i < 5; i++ {
		require.NoError(t, ClaimSpawnSlot("agent-a", 0, time.Hour, now), "claim %d with max=0", i)
	}
	require.NoError(t, ClaimSpawnSlot("agent-a", -1, time.Hour, now), "claim with max=-1")
	n, err := CountSpawnsSince("agent-a", now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, n, "an unlimited claim must not record history rows")
}

func TestClaimSpawnSlot_EnforcesCapWithinWindow(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	// Cap of 3 — three claims pass, the fourth is refused.
	for i := 1; i <= 3; i++ {
		require.NoError(t, ClaimSpawnSlot("agent-a", 3, time.Hour, now), "claim %d within cap", i)
	}
	require.ErrorIs(t, ClaimSpawnSlot("agent-a", 3, time.Hour, now), ErrSpawnRateLimited,
		"the 4th claim must be rate-limited")

	// The refused claim left the table untouched — still exactly 3.
	n, err := CountSpawnsSince("agent-a", now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n, "a rate-limited claim must not insert a row")
}

func TestClaimSpawnSlot_IsPerSpawner(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	require.NoError(t, ClaimSpawnSlot("agent-a", 1, time.Hour, now), "agent-a's one claim")
	require.ErrorIs(t, ClaimSpawnSlot("agent-a", 1, time.Hour, now), ErrSpawnRateLimited,
		"agent-a is now at its cap")
	// agent-b counts independently — a different spawner has its own
	// fresh allowance.
	require.NoError(t, ClaimSpawnSlot("agent-b", 1, time.Hour, now),
		"a different spawner must not be throttled by agent-a")
}

func TestClaimSpawnSlot_WindowExpiry(t *testing.T) {
	setupTestDB(t)
	base := time.Now().UTC()
	// An old claim, two hours ago.
	require.NoError(t, ClaimSpawnSlot("agent-a", 1, time.Hour, base.Add(-2*time.Hour)),
		"the old claim")
	// With a 1h window the old claim has aged out, so a fresh claim
	// passes despite a cap of 1 — the rolling window releases.
	require.NoError(t, ClaimSpawnSlot("agent-a", 1, time.Hour, base),
		"a claim should pass once the old one aged out of the window")
}

func TestCountSpawnsSince(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	require.NoError(t, ClaimSpawnSlot("agent-a", 10, time.Hour, now.Add(-90*time.Minute)))
	require.NoError(t, ClaimSpawnSlot("agent-a", 10, time.Hour, now.Add(-30*time.Minute)))
	require.NoError(t, ClaimSpawnSlot("agent-a", 10, time.Hour, now))

	n, err := CountSpawnsSince("agent-a", now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "only the two claims within the last hour")

	n, err = CountSpawnsSince("agent-a", now.Add(-2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n, "all three claims since two hours ago")
}
