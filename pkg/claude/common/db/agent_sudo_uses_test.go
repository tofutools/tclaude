package db

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertLimitedSudoGrant(t *testing.T, uses int64) int64 {
	t.Helper()
	const conv = "uses-aaaa-bbbb-cccc-1111"
	_, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	now := time.Now()
	id, err := InsertSudoGrant(&SudoGrant{ConvID: conv, Slug: "agent.spawn", GrantedAt: now,
		ExpiresAt: now.Add(time.Hour), GrantedBy: "test", UsesLimit: uses, UsesLeft: uses})
	require.NoError(t, err)
	return id
}

// Parallel callers go through the guarded UPDATE, so the limit holds however
// they interleave.
func TestTakeSudoUse_ParallelCallersCannotPassTheLimit(t *testing.T) {
	setupTestDB(t)
	id := insertLimitedSudoGrant(t, 3)

	var taken atomic.Int64
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			limited, ok, err := TakeSudoUse(id)
			assert.NoError(t, err)
			assert.True(t, limited)
			if ok {
				taken.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int64(3), taken.Load())

	grant, err := GetSudoGrant(id)
	require.NoError(t, err)
	assert.False(t, grant.IsActive(time.Now()), "a used-up grant is over with time left")
	held, err := HasActiveSudoGrant(grant.ConvID, grant.Slug)
	require.NoError(t, err)
	assert.False(t, held)
	active, err := LookupActiveSudoGrantID(grant.ConvID, grant.Slug)
	require.NoError(t, err)
	assert.Zero(t, active)
}

func TestReturnSudoUse_NeverPassesTheLimit(t *testing.T) {
	setupTestDB(t)
	id := insertLimitedSudoGrant(t, 2)
	_, ok, err := TakeSudoUse(id)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, ReturnSudoUse(id))
	require.NoError(t, ReturnSudoUse(id))
	grant, err := GetSudoGrant(id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), grant.UsesLeft)
}

func TestTakeSudoUse_UnlimitedGrantIsNotCounted(t *testing.T) {
	setupTestDB(t)
	id := insertLimitedSudoGrant(t, 0)
	limited, taken, err := TakeSudoUse(id)
	require.NoError(t, err)
	assert.False(t, limited)
	assert.False(t, taken)
}

// Two live Stewards hold a copy each of one stored grant, and share its count.
func TestTakeSudoUse_CopiesOfAStoredGrantShareOneCount(t *testing.T) {
	setupTestDB(t)
	id := insertLimitedSudoGrant(t, 3)
	require.NoError(t, CarrySudoGrant(id))
	grant, err := GetSudoGrant(id)
	require.NoError(t, err)

	const other = "uses-aaaa-bbbb-cccc-2222"
	otherAgent, _, err := EnsureAgentForConv(other, "test")
	require.NoError(t, err)
	stored, err := ListActiveStewardCarriedSudo()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	second, err := InheritStewardCarriedSudo(otherAgent, stored[0], "test")
	require.NoError(t, err)

	var taken int
	for _, grantID := range []int64{grant.ID, second, grant.ID, second, grant.ID, second} {
		_, ok, err := TakeSudoUse(grantID)
		require.NoError(t, err)
		if ok {
			taken++
		}
	}
	assert.Equal(t, 3, taken)
}

func TestGroupExportKeepsSudoUseLimits(t *testing.T) {
	setupTestDB(t)
	id := insertLimitedSudoGrant(t, 5)
	_, ok, err := TakeSudoUse(id)
	require.NoError(t, err)
	require.True(t, ok)
	grant, err := GetSudoGrant(id)
	require.NoError(t, err)

	d, err := Open()
	require.NoError(t, err)
	exported, err := collectSudoGrants(d, []string{grant.ConvID})
	require.NoError(t, err)
	require.Len(t, exported, 1)
	assert.Equal(t, int64(5), exported[0].UsesLimit)
	assert.Equal(t, int64(4), exported[0].UsesLeft)
}
