package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRefreshBranchLink_DoesNotWipePROnResolverMiss covers the guard in
// refreshBranchLink: `gh` is best-effort and frequently rate-limited,
// and a failed `gh pr view` is indistinguishable from "no PR" — both
// surface as PRNumber 0. Stamping that zero would blank a good PR
// snapshot off a branch the agent has since moved away from (it gets
// no further refresh). A PR-less resolution must therefore leave the
// existing snapshot intact.
func TestRefreshBranchLink_DoesNotWipePROnResolverMiss(t *testing.T) {
	setupTestDB(t)

	const repoDir = "/repo/wt"
	const branch = "feature-x"

	// A history row exists for the branch (built by an earlier scan).
	require.NoError(t, db.RebuildConvBranchHistoryScan("c1", []db.BranchObservation{
		{Branch: branch, RepoDir: repoDir},
	}))
	key := branchLinkCacheKey(repoDir, branch)

	// Resolution #1: an open PR is found — it lands on the history row.
	restore := SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			return "https://github.com/o/r", "main", 42,
				"https://github.com/o/r/pull/42", "open", true
		})
	refreshBranchLink(repoDir, branch, key)
	restore()

	rows, err := db.ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 42, rows[0].PRNumber, "PR stamped on the first resolution")

	// Resolution #2 models a rate-limited `gh`: the repo still resolves
	// (ok=true) but no PR comes back. The good snapshot must survive.
	restore = SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			return "https://github.com/o/r", "main", 0, "", "", true
		})
	refreshBranchLink(repoDir, branch, key)
	restore()

	rows, err = db.ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 42, rows[0].PRNumber, "a PR-less resolution must not wipe the snapshot")
	assert.Equal(t, "open", rows[0].PRState)
}
