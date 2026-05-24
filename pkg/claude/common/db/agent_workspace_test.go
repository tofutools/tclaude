package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentWorkspace_UpsertGetDelete covers the round trip: the
// statusbar's live snapshot stores cwd/branch + repo + PR + updated_at,
// a re-upsert overwrites in place (and bumps updated_at), and a delete
// drops the row.
func TestAgentWorkspace_UpsertGetDelete(t *testing.T) {
	setupTestDB(t)

	const conv = "ws-conv-1"

	w, err := GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.Empty(t, w.Cwd, "no row yet → zero value")

	t0 := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	require.NoError(t, UpsertAgentWorkspace(AgentWorkspace{
		ConvID:        conv,
		Cwd:           "/repo/svc/api",
		Branch:        "feature-x",
		RepoURL:       "https://github.com/o/r",
		DefaultBranch: "main",
		PRNumber:      42,
		PRURL:         "https://github.com/o/r/pull/42",
		PRState:       "open",
		UpdatedAt:     t0,
	}))
	got, err := GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.Equal(t, "/repo/svc/api", got.Cwd)
	assert.Equal(t, "feature-x", got.Branch)
	assert.Equal(t, "https://github.com/o/r", got.RepoURL)
	assert.Equal(t, "main", got.DefaultBranch)
	assert.Equal(t, 42, got.PRNumber)
	assert.Equal(t, "https://github.com/o/r/pull/42", got.PRURL)
	assert.Equal(t, "open", got.PRState)
	assert.True(t, got.UpdatedAt.Equal(t0), "updated_at round-trips")

	// Re-upsert: every field overwrites, including a clear-out of the PR
	// fields when the agent moves to the default branch.
	t1 := time.Now().Truncate(time.Microsecond)
	require.NoError(t, UpsertAgentWorkspace(AgentWorkspace{
		ConvID:        conv,
		Cwd:           "/repo/svc/api",
		Branch:        "main",
		RepoURL:       "https://github.com/o/r",
		DefaultBranch: "main",
		UpdatedAt:     t1,
	}))
	got, err = GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.Equal(t, "main", got.Branch)
	assert.Equal(t, 0, got.PRNumber, "PR fields clear when overwritten with zero values")
	assert.Empty(t, got.PRURL)
	assert.Empty(t, got.PRState)
	assert.True(t, got.UpdatedAt.Equal(t1))

	// UpdatedAt unset → caller doesn't care about the clock, so we stamp now().
	require.NoError(t, UpsertAgentWorkspace(AgentWorkspace{ConvID: conv, Cwd: "/x", Branch: "y"}))
	got, err = GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), got.UpdatedAt, 5*time.Second, "zero UpdatedAt → stamped at upsert time")

	require.NoError(t, DeleteAgentWorkspace(conv))
	got, err = GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.Empty(t, got.Cwd, "delete drops the row")

	// Empty convID is a silent no-op, not an error — the statusbar calls
	// us before the daemon has resolved a session-id for the very first
	// render in some launch paths.
	require.NoError(t, UpsertAgentWorkspace(AgentWorkspace{Cwd: "/x"}))
}
