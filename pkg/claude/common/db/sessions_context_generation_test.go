package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UpdateContextSnapshotForGeneration's contract.
//
// The returned bool is rows-affected semantics: true means THIS generation's
// row was updated. A read-through follower derives its projection from a log
// read that began before the write, so a session pruned and recreated in
// between must be left alone — and must be reported as such, not merely
// happen to be left alone.

func contextGenerationSession(t *testing.T, id, convID string, createdAt time.Time) *SessionRow {
	t.Helper()
	sess := &SessionRow{
		ID: id, ConvID: convID, TmuxSession: "pane-" + id, Status: "idle",
		Harness: "copilot", CreatedAt: createdAt,
	}
	require.NoError(t, SaveSession(sess))
	return sess
}

func TestUpdateContextSnapshotForGenerationWritesTheMatchingGeneration(t *testing.T) {
	setupTestDB(t)
	created := time.Now().Truncate(time.Second)
	sess := contextGenerationSession(t, "gen-ok", "conv-a", created)

	updated, err := UpdateContextSnapshotForGeneration(sess.ID, "conv-a", created, 50, 100, 200, 128000)
	require.NoError(t, err)
	assert.True(t, updated)

	snap, err := GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.InDelta(t, 50.0, snap.ContextPct, 0.001)
	assert.Equal(t, int64(100), snap.TokensInput)
	assert.Equal(t, int64(200), snap.TokensOutput)
	assert.Equal(t, int64(128000), snap.ContextWindowSize)

	// A second write under the same generation with an unchanged window takes
	// the guarded fast path and must still report true.
	updated, err = UpdateContextSnapshotForGeneration(sess.ID, "conv-a", created, 60, 120, 240, 128000)
	require.NoError(t, err)
	assert.True(t, updated)
	snap, err = GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(240), snap.TokensOutput)
}

// TestUpdateContextSnapshotForGenerationRefusesAStaleGeneration covers both
// halves of the guard: the fast path (window unchanged) and the projecting
// path (window changed) must each refuse a caller whose generation moved on,
// and must leave the recreated row untouched.
func TestUpdateContextSnapshotForGenerationRefusesAStaleGeneration(t *testing.T) {
	setupTestDB(t)
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	current := time.Now().Truncate(time.Second)

	sess := contextGenerationSession(t, "gen-stale", "conv-new", current)
	updated, err := UpdateContextSnapshotForGeneration(sess.ID, "conv-new", current, 10, 1, 2, 64000)
	require.NoError(t, err)
	require.True(t, updated)

	// Same window as stored → the fast path. Stale generation → refused.
	updated, err = UpdateContextSnapshotForGeneration(sess.ID, "conv-old", stale, 99, 999, 999, 64000)
	require.NoError(t, err)
	assert.False(t, updated, "the fast path must refuse a stale generation")

	// Different window → the projecting path. Stale generation → refused.
	updated, err = UpdateContextSnapshotForGeneration(sess.ID, "conv-old", stale, 99, 999, 999, 400000)
	require.NoError(t, err)
	assert.False(t, updated, "the projecting path must refuse a stale generation")

	snap, err := GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(64000), snap.ContextWindowSize, "the live row must be untouched")
	assert.Equal(t, int64(2), snap.TokensOutput)
}

// TestUpdateContextSnapshotForGenerationDoesNotFalsePositiveOnEqualValues is
// the contract case a read-back heuristic gets wrong.
//
// If success were inferred by reading the row afterwards and comparing the
// window and output tokens, a recreated generation that COINCIDENTALLY holds
// the same values would report true even though the guarded UPDATE affected
// zero rows. The bool is taken from the statement result instead, so equal
// values change nothing.
func TestUpdateContextSnapshotForGenerationDoesNotFalsePositiveOnEqualValues(t *testing.T) {
	setupTestDB(t)
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	current := time.Now().Truncate(time.Second)

	sess := contextGenerationSession(t, "gen-equal", "conv-new", current)
	// The live row already holds exactly the values the stale caller is about
	// to write, and a DIFFERENT window from the row's own — forcing the
	// projecting path while making the read-back comparison succeed.
	updated, err := UpdateContextSnapshotForGeneration(sess.ID, "conv-new", current, 42, 7, 300, 200000)
	require.NoError(t, err)
	require.True(t, updated)

	updated, err = UpdateContextSnapshotForGeneration(sess.ID, "conv-old", stale, 42, 7, 300, 200000)
	require.NoError(t, err)
	assert.False(t, updated,
		"identical values must not be reported as a write; the bool is rows-affected, not a value comparison")
}

// TestUpdateContextSnapshotForGenerationIgnoresAnEmptyProjection mirrors
// UpdateContextSnapshot's all-zero guard: nothing to say is not a write.
func TestUpdateContextSnapshotForGenerationIgnoresAnEmptyProjection(t *testing.T) {
	setupTestDB(t)
	created := time.Now().Truncate(time.Second)
	sess := contextGenerationSession(t, "gen-empty", "conv-a", created)

	updated, err := UpdateContextSnapshotForGeneration(sess.ID, "conv-a", created, 0, 0, 0, 0)
	require.NoError(t, err)
	assert.False(t, updated)
}
