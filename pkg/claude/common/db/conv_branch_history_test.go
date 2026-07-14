package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// branchByName indexes a ListConvBranchHistory result by branch name so
// assertions don't depend on row order.
func branchByName(rows []ConvBranchHistoryRow) map[string]ConvBranchHistoryRow {
	m := make(map[string]ConvBranchHistoryRow, len(rows))
	for _, r := range rows {
		m[r.Branch] = r
	}
	return m
}

// TestConvBranchHistory_ScanRebuildBasics covers a first scan: every
// observed branch lands as a 'scan' row carrying its dir + timestamps,
// and an unknown conv lists empty.
func TestConvBranchHistory_ScanRebuildBasics(t *testing.T) {
	setupTestDB(t)

	empty, err := ListConvBranchHistory("nobody")
	require.NoError(t, err)
	assert.Empty(t, empty, "unknown conv lists empty, not an error")

	t0 := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0.Add(time.Hour)},
		{Branch: "feature-x", RepoDir: "/repo", FirstSeen: t0.Add(time.Hour), LastSeen: t0.Add(2 * time.Hour)},
	}))

	rows, err := ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	by := branchByName(rows)

	main := by["main"]
	assert.Equal(t, BranchSourceScan, main.Source)
	assert.Equal(t, "/repo", main.RepoDir)
	assert.True(t, main.FirstSeen.Equal(t0), "first_seen round-trips")
	assert.True(t, main.LastSeen.Equal(t0.Add(time.Hour)), "last_seen round-trips")
	assert.Zero(t, main.PRNumber, "no PR resolved yet")
	assert.Empty(t, main.PRState)

	assert.Equal(t, "feature-x", by["feature-x"].Branch)
}

// TestConvBranchHistory_RebuildIsIdempotent runs the same observation
// set twice and asserts the row set is byte-identical — the property
// the whole "re-scan is the source of truth" design rests on.
func TestConvBranchHistory_RebuildIsIdempotent(t *testing.T) {
	setupTestDB(t)

	t0 := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	obs := []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
		{Branch: "feat", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0.Add(time.Hour)},
	}

	require.NoError(t, RebuildConvBranchHistoryScan("c1", obs))
	first, err := ListConvBranchHistory("c1")
	require.NoError(t, err)

	require.NoError(t, RebuildConvBranchHistoryScan("c1", obs))
	second, err := ListConvBranchHistory("c1")
	require.NoError(t, err)

	require.Equal(t, first, second, "re-running an identical scan converges")
}

// TestConvBranchHistory_RebuildDropsStaleScanRows covers the true-mirror
// property: a branch absent from a later scan's observations is dropped,
// rather than lingering as a monotonic accumulation.
func TestConvBranchHistory_RebuildDropsStaleScanRows(t *testing.T) {
	setupTestDB(t)

	t0 := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
		{Branch: "gone", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
	}))

	// A re-scan that no longer names "gone" drops its row.
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
	}))

	rows, err := ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "main", rows[0].Branch)
}

// TestConvBranchHistory_RebuildWithEmptyObsClearsScanKeepsHook covers
// the degenerate rebuild: a re-scan that observes no branches at all
// (a .jsonl with no branch-stamped turns) drops every 'scan' row yet
// leaves 'hook' rows — re-scan owns only what it can see.
func TestConvBranchHistory_RebuildWithEmptyObsClearsScanKeepsHook(t *testing.T) {
	setupTestDB(t)

	t0 := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
	}))
	require.NoError(t, AppendConvBranchHistoryHook("c1", "worktree-feat", "/wt"))

	require.NoError(t, RebuildConvBranchHistoryScan("c1", nil))

	rows := mustList(t, "c1")
	require.Len(t, rows, 1, "the scan row is dropped, the hook row survives")
	assert.Equal(t, "worktree-feat", rows[0].Branch)
	assert.Equal(t, BranchSourceHook, rows[0].Source)
}

// TestConvBranchHistory_RebuildPreservesPRSnapshot asserts a PR stamped
// by SetConvBranchHistoryPR survives a subsequent re-scan — the rebuild
// owns the branch set, not the PR enrichment.
func TestConvBranchHistory_RebuildPreservesPRSnapshot(t *testing.T) {
	setupTestDB(t)

	t0 := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	obs := []BranchObservation{
		{Branch: "feature-x", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
	}
	require.NoError(t, RebuildConvBranchHistoryScan("c1", obs))

	require.NoError(t, SetConvBranchHistoryPR("/repo", "feature-x", 142,
		"https://github.com/o/r/pull/142", "open"))

	// A later re-scan must not blank the PR snapshot.
	require.NoError(t, RebuildConvBranchHistoryScan("c1", obs))

	rows, err := ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 142, rows[0].PRNumber, "re-scan preserves the PR number")
	assert.Equal(t, "https://github.com/o/r/pull/142", rows[0].PRURL)
	assert.Equal(t, "open", rows[0].PRState)
}

// rowFor finds the history row with the given (repoDir, branch), or
// fails the test.
func rowFor(t *testing.T, convID, repoDir, branch string) ConvBranchHistoryRow {
	t.Helper()
	for _, r := range mustList(t, convID) {
		if r.RepoDir == repoDir && r.Branch == branch {
			return r
		}
	}
	t.Fatalf("no row for (%s, %s, %s)", convID, repoDir, branch)
	return ConvBranchHistoryRow{}
}

// TestConvBranchHistory_HookAndScanCoexist covers the two writers: the
// re-scan never deletes a 'hook' row, and a (repo_dir, branch) first
// seen by the hook is upgraded to 'scan' (keeping its earlier
// first_seen) once the .jsonl names that same pair.
func TestConvBranchHistory_HookAndScanCoexist(t *testing.T) {
	setupTestDB(t)

	// The hook records a worktree branch the re-scan will never see,
	// and "shared" in the same repo the re-scan will later name.
	require.NoError(t, AppendConvBranchHistoryHook("c1", "worktree-feat", "/wt"))
	require.NoError(t, AppendConvBranchHistoryHook("c1", "shared", "/repo"))

	hookFirstSeen := rowFor(t, "c1", "/repo", "shared").FirstSeen
	require.False(t, hookFirstSeen.IsZero(), "hook stamps first_seen")

	// A re-scan names "main" and "shared", both in /repo. t1 must be
	// strictly after hookFirstSeen so the upgrade has a genuine "later
	// scan, earlier hook" relationship to preserve — derive it from the
	// hook's recorded time rather than a hardcoded sentinel date that
	// goes stale once today catches up with it.
	t1 := hookFirstSeen.Add(time.Hour)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t1, LastSeen: t1},
		{Branch: "shared", RepoDir: "/repo", FirstSeen: t1, LastSeen: t1},
	}))

	rows := mustList(t, "c1")
	require.Len(t, rows, 3, "the hook-only worktree branch is not deleted by re-scan")

	assert.Equal(t, BranchSourceHook, rowFor(t, "c1", "/wt", "worktree-feat").Source,
		"a worktree branch the .jsonl never names stays a hook row")
	assert.Equal(t, BranchSourceScan, rowFor(t, "c1", "/repo", "main").Source)

	upgraded := rowFor(t, "c1", "/repo", "shared")
	assert.Equal(t, BranchSourceScan, upgraded.Source,
		"a hook pair the .jsonl later names is upgraded to scan")
	assert.True(t, upgraded.FirstSeen.Equal(hookFirstSeen),
		"the upgrade keeps the earlier hook first_seen")
	assert.True(t, upgraded.LastSeen.Equal(t1),
		"the upgrade adopts the later scan last_seen")
}

// TestConvBranchHistory_HookAppendKeyedByRepoDir covers AppendConvBranchHistoryHook:
// a repeated sighting of the same (repo_dir, branch) bumps last_seen in
// place, while the same branch in a different repo_dir is a distinct
// row — repo_dir is part of the key.
func TestConvBranchHistory_HookAppendKeyedByRepoDir(t *testing.T) {
	setupTestDB(t)

	firstSeen := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	require.NoError(t, appendConvBranchHistoryHookAt("c1", "feat", "/wt-a", firstSeen))
	before := rowFor(t, "c1", "/wt-a", "feat")

	// A no-op for an empty branch or conv.
	require.NoError(t, AppendConvBranchHistoryHook("c1", "", "/wt-a"))
	require.NoError(t, AppendConvBranchHistoryHook("", "feat", "/wt-a"))
	require.Len(t, mustList(t, "c1"), 1, "empty branch/conv appends nothing")

	// A repeat of the same (repo_dir, branch) bumps last_seen in place.
	later := firstSeen.Add(time.Minute)
	require.NoError(t, appendConvBranchHistoryHookAt("c1", "feat", "/wt-a", later))
	require.Len(t, mustList(t, "c1"), 1, "a same-pair repeat is one row")
	after := rowFor(t, "c1", "/wt-a", "feat")
	assert.True(t, after.FirstSeen.Equal(before.FirstSeen), "first_seen is pinned")
	assert.True(t, after.LastSeen.Equal(later), "last_seen advances to the controlled observation time")

	// The same branch in a different repo is a distinct row.
	require.NoError(t, AppendConvBranchHistoryHook("c1", "feat", "/wt-b"))
	assert.Len(t, mustList(t, "c1"), 2, "same branch, different repo_dir is a new row")
}

// TestConvBranchHistory_MultiRepoSameBranch covers the key change: one
// conversation working a branch of the same name in two repos keeps
// two distinct rows, and a later observation of one of them merges
// into its row rather than duplicating.
func TestConvBranchHistory_MultiRepoSameBranch(t *testing.T) {
	setupTestDB(t)

	tA := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo-a", FirstSeen: tA, LastSeen: tA},
		{Branch: "main", RepoDir: "/repo-b", FirstSeen: tB, LastSeen: tB},
	}))

	rows := mustList(t, "c1")
	require.Len(t, rows, 2, "branch `main` in two repos is two rows, not one")
	assert.Equal(t, "/repo-a", rowFor(t, "c1", "/repo-a", "main").RepoDir)
	assert.Equal(t, "/repo-b", rowFor(t, "c1", "/repo-b", "main").RepoDir)

	// A later re-scan re-observes repo-a's main further along — it
	// merges into that row (last_seen advances), still two rows total.
	tA2 := tA.Add(3 * time.Hour)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo-a", FirstSeen: tA, LastSeen: tA2},
		{Branch: "main", RepoDir: "/repo-b", FirstSeen: tB, LastSeen: tB},
	}))
	require.Len(t, mustList(t, "c1"), 2, "re-observation merges, not duplicates")
	rowA := rowFor(t, "c1", "/repo-a", "main")
	assert.True(t, rowA.FirstSeen.Equal(tA), "first_seen pinned to the earliest")
	assert.True(t, rowA.LastSeen.Equal(tA2), "last_seen advanced to the latest")
}

// TestConvBranchHistory_RebuildMergesSameBatchDuplicates covers the
// pre-merge inside RebuildConvBranchHistoryScan: two observations of
// the same (repo_dir, branch) in ONE batch fold into a single row with
// min/max timestamps — the INSERT...ON CONFLICT alone could not, as it
// can't see a row inserted earlier in the same transaction.
func TestConvBranchHistory_RebuildMergesSameBatchDuplicates(t *testing.T) {
	setupTestDB(t)

	early := time.Date(2026, 5, 17, 8, 0, 0, 0, time.UTC)
	late := time.Date(2026, 5, 17, 20, 0, 0, 0, time.UTC)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "feat", RepoDir: "/repo", FirstSeen: late, LastSeen: late},
		{Branch: "feat", RepoDir: "/repo", FirstSeen: early, LastSeen: early},
	}))

	rows := mustList(t, "c1")
	require.Len(t, rows, 1, "same-batch dups fold into one row")
	assert.True(t, rows[0].FirstSeen.Equal(early), "first_seen is the batch min")
	assert.True(t, rows[0].LastSeen.Equal(late), "last_seen is the batch max")
}

// TestConvBranchHistory_SetPRMatchesByRepoDirAndBranch covers the PR
// stamp: it hits every conv sharing a (repo_dir, branch) and ignores
// rows whose dir or branch differs.
func TestConvBranchHistory_SetPRMatchesByRepoDirAndBranch(t *testing.T) {
	setupTestDB(t)

	t0 := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	for _, conv := range []string{"c1", "c2"} {
		require.NoError(t, RebuildConvBranchHistoryScan(conv, []BranchObservation{
			{Branch: "feature-x", RepoDir: "/repo", FirstSeen: t0, LastSeen: t0},
		}))
	}
	// A same-named branch in a different repo must not be touched.
	require.NoError(t, RebuildConvBranchHistoryScan("c3", []BranchObservation{
		{Branch: "feature-x", RepoDir: "/other", FirstSeen: t0, LastSeen: t0},
	}))

	require.NoError(t, SetConvBranchHistoryPR("/repo", "feature-x", 7,
		"https://github.com/o/r/pull/7", "merged"))

	for _, conv := range []string{"c1", "c2"} {
		row := mustList(t, conv)[0]
		assert.Equal(t, 7, row.PRNumber, conv+" picks up the shared PR")
		assert.Equal(t, "merged", row.PRState)
	}
	assert.Zero(t, mustList(t, "c3")[0].PRNumber,
		"a same-named branch in another repo is untouched")

	// An empty repoDir/branch is a no-op.
	require.NoError(t, SetConvBranchHistoryPR("", "feature-x", 9, "u", "open"))
	require.NoError(t, SetConvBranchHistoryPR("/repo", "", 9, "u", "open"))
}

// TestConvBranchHistory_Delete covers the eviction path.
func TestConvBranchHistory_Delete(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, AppendConvBranchHistoryHook("c1", "feat", "/wt"))
	require.NoError(t, AppendConvBranchHistoryHook("c2", "feat", "/wt"))

	require.NoError(t, DeleteConvBranchHistory("c1"))
	assert.Empty(t, mustList(t, "c1"), "c1 history dropped")
	assert.Len(t, mustList(t, "c2"), 1, "c2 history untouched")
}

// mustList is a ListConvBranchHistory that fails the test on error.
func mustList(t *testing.T, convID string) []ConvBranchHistoryRow {
	t.Helper()
	rows, err := ListConvBranchHistory(convID)
	require.NoError(t, err)
	return rows
}
