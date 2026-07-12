package convops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The follower's core contract is restart-equivalence: the entry it
// produces from reading only appended bytes must be byte-identical to a
// full parseJSONLSession of the same file. These tests assert that property
// directly (comparing follower output to a full reparse) across append,
// shrink, inode swap, partial trailing line, oversized record, in-place
// rewrite-grows, and randomized chunk boundaries.

const followerTestConvID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// userLine builds a user-turn record with the content near the END of the
// JSON object, so a short tail-anchor window still covers the prompt text
// (used by the rewrite-grows anchor test).
func userLine(branch, ts, content string) string {
	return fmt.Sprintf(
		`{"type":"user","cwd":"/proj","gitBranch":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`,
		branch, ts, content) + "\n"
}

func assistantLine(branch, ts string) string {
	return fmt.Sprintf(
		`{"type":"assistant","cwd":"/proj","gitBranch":%q,"timestamp":%q,"message":{"role":"assistant","content":"ok"}}`,
		branch, ts) + "\n"
}

func titleLine(title string) string {
	return fmt.Sprintf(`{"type":"custom-title","customTitle":%q}`, title) + "\n"
}

func summaryLine(summary string) string {
	return fmt.Sprintf(`{"type":"summary","summary":%q}`, summary) + "\n"
}

// followerFixture is a realistic multi-record transcript exercising every
// field class: head-only (first prompt / cwd / startup branch), last-seen
// (summary, custom title, current branch), and additive (branch history).
var followerFixture = []string{
	userLine("main", "2026-03-01T10:00:00Z", "first prompt"),
	assistantLine("main", "2026-03-01T10:01:00Z"),
	titleLine("my-agent"),
	userLine("feature-a", "2026-03-01T11:00:00Z", "second"),
	summaryLine("did stuff"),
	assistantLine("feature-a", "2026-03-01T11:02:00Z"),
	userLine("feature-b", "2026-03-01T12:00:00Z", "third"),
	assistantLine("feature-b", "2026-03-01T12:02:00Z"),
}

func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// normalizeEntry sorts BranchHistory so two entries built from the same
// records compare equal regardless of Go's unstable map iteration order.
func normalizeEntry(e *SessionEntry) {
	if e == nil {
		return
	}
	sort.Slice(e.BranchHistory, func(i, j int) bool {
		if e.BranchHistory[i].Branch != e.BranchHistory[j].Branch {
			return e.BranchHistory[i].Branch < e.BranchHistory[j].Branch
		}
		return e.BranchHistory[i].RepoDir < e.BranchHistory[j].RepoDir
	})
}

// assertMatchesFullReparse is the equivalence oracle: the follower's entry
// for the current file state must equal a full parseJSONLSession, field for
// field (MessageCount parity included — it is always 0, asserted by the
// full struct compare).
func assertMatchesFullReparse(t *testing.T, path string, gotEntry *SessionEntry, gotComplete bool) {
	t.Helper()
	wantEntry, wantComplete := parseJSONLSession(path, followerTestConvID)
	normalizeEntry(gotEntry)
	normalizeEntry(wantEntry)
	require.Equal(t, wantEntry, gotEntry, "follower entry must equal a full reparse")
	require.Equal(t, wantComplete, gotComplete, "follower scanComplete must equal a full reparse")
}

// TestConvFollower_AppendMatchesFullReparse writes the fixture one record at
// a time and asserts, after every append, that the incrementally-followed
// entry equals a full reparse of the file-so-far.
func TestConvFollower_AppendMatchesFullReparse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(followerFixture[0]), 0o600))

	f := newConvFollower(followerTestConvID)
	entry, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)

	for _, rec := range followerFixture[1:] {
		appendToFile(t, path, rec)
		entry, complete, err = f.refresh(path, statOf(t, path))
		require.NoError(t, err)
		assertMatchesFullReparse(t, path, entry, complete)
	}

	// Sanity: the fully-followed entry carries the expected projection.
	require.Equal(t, "first prompt", entry.FirstPrompt)
	require.Equal(t, "my-agent", entry.CustomTitle)
	require.Equal(t, "did stuff", entry.Summary)
	require.Equal(t, "feature-b", entry.GitBranch, "last-wins current branch")
	require.Equal(t, "main", entry.GitBranchStartup, "first-wins startup branch")
	require.Len(t, entry.BranchHistory, 3)
}

// TestConvFollower_PartialTrailingLine appends a record in two halves split
// mid-line. The follower must not consume the torn tail until the newline
// arrives, and must never diverge from a full reparse.
func TestConvFollower_PartialTrailingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(followerFixture[0]), 0o600))

	f := newConvFollower(followerTestConvID)
	_, _, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	offsetAfterFirst := f.offset

	// Append the first half of a record — no trailing newline yet.
	full := userLine("feature-a", "2026-03-01T11:00:00Z", "second")
	half := full[:len(full)/2]
	appendToFile(t, path, half)
	entry, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)
	require.Equal(t, offsetAfterFirst, f.offset, "torn tail is not committed to the cursor")
	require.Equal(t, "first prompt", entry.FirstPrompt, "no second prompt visible yet")

	// Complete the record; now it must be visible.
	appendToFile(t, path, full[len(full)/2:])
	entry, complete, err = f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)
	require.Greater(t, f.offset, offsetAfterFirst, "completed record advances the cursor")
}

// TestConvFollower_ShrinkTriggersFullReparse: a file that shrank below the
// cursor cannot be trusted for an incremental read — the follower must
// full-reparse and still match.
func TestConvFollower_ShrinkTriggersFullReparse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(followerFixture[0]+followerFixture[1]+followerFixture[2]), 0o600))

	f := newConvFollower(followerTestConvID)
	_, _, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	require.Greater(t, f.offset, int64(0))

	// Replace with strictly smaller content (a different, shorter conv).
	shrunk := userLine("release", "2026-04-01T09:00:00Z", "brand new")
	require.NoError(t, os.WriteFile(path, []byte(shrunk), 0o600))

	entry, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)
	require.Equal(t, "brand new", entry.FirstPrompt, "shrink forced a full reparse of the new content")
	require.Equal(t, "release", entry.GitBranchStartup)
}

// TestConvFollower_InodeSwapTriggersFullReparse: a same-path replace that
// swaps the inode (rotation / atomic replace) invalidates the cursor even
// when the new file is larger.
func TestConvFollower_InodeSwapTriggersFullReparse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(followerFixture[0]), 0o600))

	f := newConvFollower(followerTestConvID)
	_, _, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	primedInode := statOf(t, path)

	// Atomic replace: write a NEW, larger file to a temp name and rename
	// over the path. rename gives the path a different inode.
	tmp := path + ".new"
	bigger := userLine("release", "2026-04-01T09:00:00Z", "replaced") +
		assistantLine("release", "2026-04-01T09:01:00Z") +
		userLine("release", "2026-04-01T09:02:00Z", "and more")
	require.NoError(t, os.WriteFile(tmp, []byte(bigger), 0o600))
	require.NoError(t, os.Rename(tmp, path))
	require.False(t, os.SameFile(primedInode, statOf(t, path)), "precondition: inode changed")

	entry, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)
	require.Equal(t, "replaced", entry.FirstPrompt, "inode swap forced a full reparse")
}

// TestConvFollower_RewriteGrowsLargerAnchorCatches is the case the size /
// inode guards alone would miss: an in-place rewrite (same inode) that
// changes bytes BEFORE the cursor and ends LARGER than the cursor. A naive
// seek-to-offset would fold the newly-appended record onto a stale head
// (keeping the old first-wins FirstPrompt). The tail-anchor check detects
// the changed committed bytes and forces a full reparse.
func TestConvFollower_RewriteGrowsLargerAnchorCatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")

	// v1: a single user record whose prompt text sits near the end of the
	// JSON, so the tail anchor window covers it.
	v1 := userLine("main", "2026-03-01T10:00:00Z", "AAA")
	require.NoError(t, os.WriteFile(path, []byte(v1), 0o600))

	f := newConvFollower(followerTestConvID)
	entry, _, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	require.Equal(t, "AAA", entry.FirstPrompt)
	primedInode := statOf(t, path)

	// In-place rewrite (os.WriteFile truncates the SAME inode): change the
	// head prompt AAA→BBB (same length, so the record boundary at the old
	// offset still aligns) and append a second record, so the file grows
	// past the old cursor.
	v2 := userLine("main", "2026-03-01T10:00:00Z", "BBB") +
		userLine("main", "2026-03-01T10:05:00Z", "CCC")
	require.NoError(t, os.WriteFile(path, []byte(v2), 0o600))
	require.True(t, os.SameFile(primedInode, statOf(t, path)),
		"precondition: same inode (in-place rewrite, not a replace)")
	require.Greater(t, statOf(t, path).Size(), f.offset,
		"precondition: rewrite ends larger than the cursor")

	entry, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)
	require.Equal(t, "BBB", entry.FirstPrompt,
		"anchor mismatch forced a full reparse; the stale first-wins prompt was discarded")
}

// TestConvFollower_OversizedRecordSkipped: a record past the line cap is
// skipped (not a hard failure), the row is still built from the other
// records, and the scan is marked incomplete (so the destructive
// branch-history rebuild is skipped). Equivalence with a full reparse holds.
func TestConvFollower_OversizedRecordSkipped(t *testing.T) {
	orig := maxJSONLLineBytes
	maxJSONLLineBytes = 256
	t.Cleanup(func() { maxJSONLLineBytes = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(followerFixture[0]), 0o600))

	f := newConvFollower(followerTestConvID)
	_, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	require.True(t, complete, "small first record scans complete")

	// Append an oversized record (a huge pasted blob) followed by a normal one.
	huge := make([]byte, maxJSONLLineBytes*2)
	for i := range huge {
		huge[i] = 'x'
	}
	appendToFile(t, path, userLine("main", "2026-03-01T11:00:00Z", string(huge)))
	appendToFile(t, path, userLine("main", "2026-03-01T12:00:00Z", "after huge"))

	entry, complete, err := f.refresh(path, statOf(t, path))
	require.NoError(t, err)
	assertMatchesFullReparse(t, path, entry, complete)
	require.False(t, complete, "an oversized-skipped scan is incomplete for rebuild purposes")
	require.Equal(t, "first prompt", entry.FirstPrompt, "the row is still built from readable records")
}

// TestConvFollower_RestartEquivalenceProperty drives the follower through
// many different chunk-boundary schedules (including splits that land
// mid-line) and asserts the followed entry equals a full reparse at every
// step and at the end — the property the whole design rests on.
func TestConvFollower_RestartEquivalenceProperty(t *testing.T) {
	full := ""
	for _, r := range followerFixture {
		full += r
	}

	// A spread of chunk sizes: 1 forces maximal mid-line tearing; primes
	// and non-divisors land boundaries at irregular places; a size larger
	// than the file delivers it whole.
	for _, chunk := range []int{1, 3, 7, 13, 50, 200, len(full) + 1} {
		t.Run(fmt.Sprintf("chunk-%d", chunk), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, followerTestConvID+".jsonl")
			require.NoError(t, os.WriteFile(path, nil, 0o600))

			f := newConvFollower(followerTestConvID)
			for off := 0; off < len(full); off += chunk {
				end := min(off+chunk, len(full))
				appendToFile(t, path, full[off:end])
				entry, complete, err := f.refresh(path, statOf(t, path))
				require.NoError(t, err)
				assertMatchesFullReparse(t, path, entry, complete)
			}
		})
	}
}

// TestConvFollower_ClearRotationIndependentPaths models a /clear: the
// conversation continues under a NEW conv-id / path. The monitor keys
// followers by path, so the two files are followed independently and each
// yields its own correct row. (Verified at the DB layer via ReindexFile.)
func TestConvFollower_ClearRotationIndependentPaths(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()

	oldID := "11111111-1111-1111-1111-111111111111"
	newID := "22222222-2222-2222-2222-222222222222"
	oldPath := filepath.Join(dir, oldID+".jsonl")
	newPath := filepath.Join(dir, newID+".jsonl")

	require.NoError(t, os.WriteFile(oldPath,
		[]byte(userLine("main", "2026-03-01T10:00:00Z", "before clear")), 0o600))
	// A fresh /clear conv starts with a summary marker (as real CC writes).
	require.NoError(t, os.WriteFile(newPath,
		[]byte(summaryLine("session "+newID)+userLine("main", "2026-03-01T10:10:00Z", "after clear")), 0o600))

	oldF := NewConvFollower(oldPath)
	newF := NewConvFollower(newPath)
	require.NotNil(t, oldF.ReindexFile(oldPath))
	require.NotNil(t, newF.ReindexFile(newPath))

	oldRow, err := db.GetConvIndex(oldID)
	require.NoError(t, err)
	require.NotNil(t, oldRow)
	assert.Equal(t, "before clear", oldRow.FirstPrompt)

	newRow, err := db.GetConvIndex(newID)
	require.NoError(t, err)
	require.NotNil(t, newRow)
	assert.Equal(t, "after clear", newRow.FirstPrompt)
}

// TestConvFollower_ReindexFileDeletesOnMissing: the exported wrapper is
// self-cleaning — an os.Stat miss evicts the conv_index row, matching
// ScanAndUpsertFile.
func TestConvFollower_ReindexFileDeletesOnMissing(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path,
		[]byte(userLine("main", "2026-03-01T10:00:00Z", "hello")), 0o600))

	f := NewConvFollower(path)
	require.NotNil(t, f.ReindexFile(path))
	row, err := db.GetConvIndex(followerTestConvID)
	require.NoError(t, err)
	require.NotNil(t, row)

	require.NoError(t, os.Remove(path))
	require.Nil(t, f.ReindexFile(path))
	row, err = db.GetConvIndex(followerTestConvID)
	require.NoError(t, err)
	require.Nil(t, row, "a removed file's row is evicted")
}

// TestConvFollower_IncrementalBranchHistoryMatchesFull asserts the DB-level
// branch-history rebuild through the incremental path equals a one-shot full
// scan — the additive fold survives being computed across ticks.
func TestConvFollower_IncrementalBranchHistoryMatchesFull(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, followerTestConvID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(followerFixture[0]), 0o600))

	f := NewConvFollower(path)
	require.NotNil(t, f.ReindexFile(path))
	for _, rec := range followerFixture[1:] {
		appendToFile(t, path, rec)
		f.ReindexFile(path)
	}

	got := branchSet(t, followerTestConvID)
	require.Len(t, got, 3, "all three branches captured incrementally")
	for _, b := range []string{"main", "feature-a", "feature-b"} {
		assert.Contains(t, got, b)
		assert.Equal(t, db.BranchSourceScan, got[b].Source)
	}
}
