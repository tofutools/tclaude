package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentWorkdir_UpsertGetDelete covers the round trip: a v28-shaped
// row stores the edit dir plus its git worktree root + branch, an
// upsert overwrites in place, and a delete drops the row.
func TestAgentWorkdir_UpsertGetDelete(t *testing.T) {
	setupTestDB(t)

	const conv = "wd-conv-1"

	// No row yet → zero value, nil error.
	w, err := GetAgentWorkdir(conv)
	require.NoError(t, err)
	assert.Empty(t, w.Dir)

	require.NoError(t, UpsertAgentWorkdir(conv,
		"/repo/svc/api/pkg", "/repo/svc/api", "feature-x"))
	w, err = GetAgentWorkdir(conv)
	require.NoError(t, err)
	assert.Equal(t, "/repo/svc/api/pkg", w.Dir)
	assert.Equal(t, "/repo/svc/api", w.WorktreeRoot)
	assert.Equal(t, "feature-x", w.Branch)

	// An upsert overwrites every field in place.
	require.NoError(t, UpsertAgentWorkdir(conv,
		"/repo/svc/web/src", "/repo/svc/web", "web-feature"))
	w, err = GetAgentWorkdir(conv)
	require.NoError(t, err)
	assert.Equal(t, "/repo/svc/web", w.WorktreeRoot)
	assert.Equal(t, "web-feature", w.Branch)

	require.NoError(t, DeleteAgentWorkdir(conv))
	w, err = GetAgentWorkdir(conv)
	require.NoError(t, err)
	assert.Empty(t, w.Dir)
}

// TestHealAgentWorkdirGit covers the backfill path: a stale row (no
// worktree_root, as a pre-v28 hook left it) gets its git info filled
// in, the heal is a no-op once the row already carries a root, and an
// empty argument is a silent no-op.
func TestHealAgentWorkdirGit(t *testing.T) {
	setupTestDB(t)

	const conv = "wd-heal-1"

	// A pre-v28-shaped row: edit dir only, no worktree_root/branch.
	require.NoError(t, UpsertAgentWorkdir(conv, "/repo/deep/sub", "", ""))

	// Heal backfills the git root + branch without touching the dir.
	require.NoError(t, HealAgentWorkdirGit(conv, "/repo", "main"))
	w, err := GetAgentWorkdir(conv)
	require.NoError(t, err)
	assert.Equal(t, "/repo/deep/sub", w.Dir, "heal leaves the edit dir untouched")
	assert.Equal(t, "/repo", w.WorktreeRoot)
	assert.Equal(t, "main", w.Branch)

	// The `worktree_root = ''` guard makes a second heal a no-op — a
	// stale heal must never clobber an already-populated row.
	require.NoError(t, HealAgentWorkdirGit(conv, "/other-repo", "other"))
	w, err = GetAgentWorkdir(conv)
	require.NoError(t, err)
	assert.Equal(t, "/repo", w.WorktreeRoot, "guard: heal does not overwrite a set root")
	assert.Equal(t, "main", w.Branch)

	// Empty convID / worktreeRoot is a silent no-op, not an error.
	require.NoError(t, HealAgentWorkdirGit("", "/x", "b"))
	require.NoError(t, HealAgentWorkdirGit(conv, "", "b"))
}
