package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRepoLinksViewUsesFreshestPRStateAcrossSources(t *testing.T) {
	now := time.Now()
	const prURL = "https://github.com/o/r/pull/42"

	links := repoLinksView{
		BranchPRURL:      prURL,
		BranchPRState:    "open",
		StartupPRURL:     prURL,
		StartupPRState:   "open",
		branchPRUpdated:  now.Add(-time.Minute),
		startupPRUpdated: now.Add(-time.Minute),
	}.withPresentedPRs([]presentedPRView{{
		URL:       "https://github.com/O/R/pull/42/files",
		State:     "merged",
		updatedAt: now,
	}})

	assert.Equal(t, "merged", links.BranchPRState,
		"a fresher presented result owns the duplicate branch badge state")
	assert.Equal(t, "merged", links.StartupPRState,
		"a fresher presented result owns the duplicate startup badge state")

	links = repoLinksView{
		BranchPRURL:     prURL,
		BranchPRState:   "merged",
		branchPRUpdated: now.Add(-time.Minute),
	}.withPresentedPRs([]presentedPRView{{
		URL:       prURL,
		State:     "open",
		updatedAt: now,
	}})

	assert.Equal(t, "merged", links.BranchPRState,
		"a newer open observation cannot regress a merged branch result")

	index := make(prStateIndex)
	index.add(prURL, "merged", now.Add(-time.Minute))
	index.add(prURL, "open", now)
	links = repoLinksView{
		PresentedPRs: []presentedPRView{{
			URL:       prURL,
			State:     "open",
			updatedAt: now,
		}},
	}.withFreshestPRStates(index)
	assert.Equal(t, "merged", links.PresentedPRs[0].State,
		"a newer open observation cannot displace merged in the cross-row index")
}

func TestBranchLinksForPartsUsesFreshestWorkspaceOrBranchState(t *testing.T) {
	now := time.Now()
	const prURL = "https://github.com/o/r/pull/42"
	loc := agentLocationView{
		CurrentDir:    "/repo",
		StartupDir:    "/repo",
		Branch:        "feature",
		StartupBranch: "feature",
	}
	ws := db.AgentWorkspace{
		ConvID:        "conv",
		Cwd:           "/repo",
		Branch:        "feature",
		RepoURL:       "https://github.com/o/r",
		DefaultBranch: "main",
		PRNumber:      42,
		PRURL:         prURL,
		PRState:       "open",
		UpdatedAt:     now.Add(-time.Minute),
	}

	links := branchLinksForParts("conv", loc, ws,
		func(string, string) (string, int, string, string, time.Time) {
			return "https://github.com/o/r/compare/main...feature",
				42, prURL, "merged", now
		})

	assert.Equal(t, "merged", links.BranchPRState,
		"a stale workspace render cannot regress a fresher branch-cache state")
	assert.Equal(t, "merged", links.StartupPRState)

	ws.PRState = "merged"
	ws.UpdatedAt = now
	links = branchLinksForParts("conv", loc, ws,
		func(string, string) (string, int, string, string, time.Time) {
			return "https://github.com/o/r/compare/main...feature",
				42, prURL, "open", now.Add(-time.Minute)
		})
	assert.Equal(t, "merged", links.BranchPRState,
		"a fresher workspace result owns the state")
	assert.Equal(t, "merged", links.StartupPRState)
}

// resolverReturning installs a git-info resolver fake that reports a
// fixed PR for any branch, and returns a restore closure.
func resolverReturning(prNumber int, prURL, prState string) func() {
	return SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			return "https://github.com/o/r", "main", prNumber, prURL, prState, true
		})
}

// TestRefreshBranchLink_PRStampGatedByFlag covers the feature flag:
// with branchHistoryPREnrichment off (the production default) a
// resolved PR is NOT stamped onto conv_branch_history; with it on, it
// is. The branch row itself exists either way — only the PR columns
// are gated.
func TestRefreshBranchLink_PRStampGatedByFlag(t *testing.T) {
	setupTestDB(t)

	const repoDir = "/repo/wt"
	const branch = "feature-x"
	require.NoError(t, db.RebuildConvBranchHistoryScan("c1", []db.BranchObservation{
		{Branch: branch, RepoDir: repoDir},
	}))
	key := branchLinkCacheKey(repoDir, branch)

	// Flag off (default): a resolved PR must not reach the history row.
	restoreOff := SetBranchHistoryPREnrichmentForTest(false)
	restoreResolver := resolverReturning(42, "https://github.com/o/r/pull/42", "open")
	refreshBranchLink(repoDir, branch, key)
	restoreResolver()
	restoreOff()

	rows, err := db.ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Zero(t, rows[0].PRNumber, "flag off: PR is not stamped")

	// Flag on: the same resolution now lands.
	restoreOn := SetBranchHistoryPREnrichmentForTest(true)
	restoreResolver = resolverReturning(42, "https://github.com/o/r/pull/42", "open")
	refreshBranchLink(repoDir, branch, key)
	restoreResolver()
	restoreOn()

	rows, err = db.ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 42, rows[0].PRNumber, "flag on: PR is stamped")
}

// TestRefreshBranchLink_DoesNotWipePROnResolverMiss covers the guard in
// refreshBranchLink: `gh` is best-effort and frequently rate-limited,
// and a failed `gh pr view` is indistinguishable from "no PR" — both
// surface as PRNumber 0. Stamping that zero would blank a good PR
// snapshot off a branch the agent has since moved away from (it gets
// no further refresh). A PR-less resolution must therefore leave the
// existing snapshot intact.
func TestRefreshBranchLink_DoesNotWipePROnResolverMiss(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetBranchHistoryPREnrichmentForTest(true))

	const repoDir = "/repo/wt"
	const branch = "feature-x"

	// A history row exists for the branch (built by an earlier scan).
	require.NoError(t, db.RebuildConvBranchHistoryScan("c1", []db.BranchObservation{
		{Branch: branch, RepoDir: repoDir},
	}))
	key := branchLinkCacheKey(repoDir, branch)

	// Resolution #1: an open PR is found — it lands on the history row.
	restore := resolverReturning(42, "https://github.com/o/r/pull/42", "open")
	refreshBranchLink(repoDir, branch, key)
	restore()

	rows, err := db.ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 42, rows[0].PRNumber, "PR stamped on the first resolution")

	// Resolution #2 models a rate-limited `gh`: the repo still resolves
	// (ok=true) but no PR comes back. The good snapshot must survive.
	restore = resolverReturning(0, "", "")
	refreshBranchLink(repoDir, branch, key)
	restore()

	rows, err = db.ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 42, rows[0].PRNumber, "a PR-less resolution must not wipe the snapshot")
	assert.Equal(t, "open", rows[0].PRState)
}

func TestPRStateFromGHFoldsDraftIntoItsOwnState(t *testing.T) {
	assert.Equal(t, "draft", prStateFromGH("OPEN", true),
		"an open draft gets its own state so the badge is not open's green")
	assert.Equal(t, "open", prStateFromGH("OPEN", false))
	// GitHub keeps isDraft set on some closed drafts. A terminal state is
	// the more important thing to show, and draft is an open-only concept.
	assert.Equal(t, "closed", prStateFromGH("CLOSED", true))
	assert.Equal(t, "merged", prStateFromGH("MERGED", false))
	assert.Equal(t, "", prStateFromGH("  ", true))
}
