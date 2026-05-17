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

// TestConvBranchHistory_HookAndScanCoexist covers the two writers: the
// re-scan never deletes a 'hook' row, and a branch first seen by the
// hook is upgraded to 'scan' (keeping its earlier first_seen) once the
// .jsonl names it.
func TestConvBranchHistory_HookAndScanCoexist(t *testing.T) {
	setupTestDB(t)

	// The hook records two worktree branches before any re-scan.
	require.NoError(t, AppendConvBranchHistoryHook("c1", "worktree-feat", "/wt"))
	require.NoError(t, AppendConvBranchHistoryHook("c1", "shared-branch", "/wt"))

	hookRows, err := ListConvBranchHistory("c1")
	require.NoError(t, err)
	require.Len(t, hookRows, 2)
	hookFirstSeen := branchByName(hookRows)["shared-branch"].FirstSeen
	require.False(t, hookFirstSeen.IsZero(), "hook stamps first_seen")

	// A re-scan names only "main" and "shared-branch".
	t1 := time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC)
	require.NoError(t, RebuildConvBranchHistoryScan("c1", []BranchObservation{
		{Branch: "main", RepoDir: "/repo", FirstSeen: t1, LastSeen: t1},
		{Branch: "shared-branch", RepoDir: "/repo", FirstSeen: t1, LastSeen: t1},
	}))

	by := branchByName(mustList(t, "c1"))
	require.Len(t, by, 3, "the hook-only worktree branch is not deleted by re-scan")

	assert.Equal(t, BranchSourceHook, by["worktree-feat"].Source,
		"a worktree branch the .jsonl never names stays a hook row")
	assert.Equal(t, BranchSourceScan, by["main"].Source)

	upgraded := by["shared-branch"]
	assert.Equal(t, BranchSourceScan, upgraded.Source,
		"a hook branch the .jsonl later names is upgraded to scan")
	assert.True(t, upgraded.FirstSeen.Equal(hookFirstSeen),
		"the upgrade keeps the earlier hook first_seen")
	assert.True(t, upgraded.LastSeen.Equal(t1),
		"the upgrade adopts the later scan last_seen")
}

// TestConvBranchHistory_HookAppendBumpsLastSeen covers AppendConvBranchHistoryHook's
// conflict path: a repeated sighting keeps first_seen but advances
// last_seen and repo_dir.
func TestConvBranchHistory_HookAppendBumpsLastSeen(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, AppendConvBranchHistoryHook("c1", "feat", "/wt-a"))
	before := mustList(t, "c1")[0]

	// A no-op for an empty branch or conv.
	require.NoError(t, AppendConvBranchHistoryHook("c1", "", "/wt"))
	require.NoError(t, AppendConvBranchHistoryHook("", "feat", "/wt"))
	require.Len(t, mustList(t, "c1"), 1, "empty branch/conv appends nothing")

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, AppendConvBranchHistoryHook("c1", "feat", "/wt-b"))
	after := mustList(t, "c1")[0]

	assert.True(t, after.FirstSeen.Equal(before.FirstSeen), "first_seen is pinned")
	assert.False(t, after.LastSeen.Before(before.LastSeen), "last_seen advances")
	assert.Equal(t, "/wt-b", after.RepoDir, "repo_dir follows the latest sighting")
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
