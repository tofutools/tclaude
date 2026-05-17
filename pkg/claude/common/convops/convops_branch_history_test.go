package convops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// branchSet collapses a branch-history result to a name→row map.
func branchSet(t *testing.T, convID string) map[string]db.ConvBranchHistoryRow {
	t.Helper()
	rows, err := db.ListConvBranchHistory(convID)
	require.NoError(t, err, "ListConvBranchHistory")
	m := make(map[string]db.ConvBranchHistoryRow, len(rows))
	for _, r := range rows {
		m[r.Branch] = r
	}
	return m
}

// jsonlWithBranchHops is a .jsonl whose turns hop across three branches.
// The custom-title and summary turns land before the final branch hop:
// under the old early-exit scan (which stopped once title+summary+
// prompt+cwd were all set) "feature-b" would never be reached. The scan
// now runs to EOF, so all three branches are captured.
const jsonlWithBranchHops = `{"type":"user","cwd":"/proj","gitBranch":"main","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","cwd":"/proj","gitBranch":"main","message":{"role":"assistant","content":"ok"},"timestamp":"2026-03-01T10:05:00Z"}
{"type":"custom-title","customTitle":"my-agent"}
{"type":"user","cwd":"/proj","gitBranch":"feature-a","message":{"role":"user","content":"work"},"timestamp":"2026-03-01T11:00:00Z"}
{"type":"summary","summary":"did stuff"}
{"type":"user","cwd":"/proj","gitBranch":"feature-b","message":{"role":"user","content":"more"},"timestamp":"2026-03-01T12:00:00Z"}
`

// TestLoadSessionsIndex_BuildsBranchHistory covers the scan-path wiring:
// LoadSessionsIndex feeds parseJSONLSession's branch set into
// conv_branch_history, including a branch that only appears after the
// summary turn.
func TestLoadSessionsIndex_BuildsBranchHistory(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, sessionID+".jsonl"), []byte(jsonlWithBranchHops), 0o600))

	_, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex")

	by := branchSet(t, sessionID)
	require.Len(t, by, 3, "all three branches captured, including the post-summary hop")
	for _, b := range []string{"main", "feature-a", "feature-b"} {
		assert.Contains(t, by, b)
		assert.Equal(t, db.BranchSourceScan, by[b].Source, b+" is a scan row")
		assert.Equal(t, "/proj", by[b].RepoDir, b+" carries the turn's cwd")
	}

	// main spans two turns — first/last seen bracket them.
	main := by["main"]
	assert.Equal(t, "2026-03-01T10:00:00Z", main.FirstSeen.UTC().Format("2006-01-02T15:04:05Z"))
	assert.Equal(t, "2026-03-01T10:05:00Z", main.LastSeen.UTC().Format("2006-01-02T15:04:05Z"))
}

// TestLoadSessionsIndex_BranchHistoryRebuildIsIdempotent asserts a
// forced re-scan converges to the same branch-history rows.
func TestLoadSessionsIndex_BranchHistoryRebuildIsIdempotent(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, sessionID+".jsonl"), []byte(jsonlWithBranchHops), 0o600))

	_, err := LoadSessionsIndexWithOptions(tmpDir, LoadSessionsIndexOptions{ForceRescan: true})
	require.NoError(t, err)
	first, err := db.ListConvBranchHistory(sessionID)
	require.NoError(t, err)

	_, err = LoadSessionsIndexWithOptions(tmpDir, LoadSessionsIndexOptions{ForceRescan: true})
	require.NoError(t, err)
	second, err := db.ListConvBranchHistory(sessionID)
	require.NoError(t, err)

	require.Equal(t, first, second, "a re-scan of the same .jsonl converges")
}

// TestLoadSessionsIndex_BranchHistoryEvictedWithConv covers the
// self-healing eviction: when a .jsonl disappears, its branch history
// is dropped alongside the conv_index row.
func TestLoadSessionsIndex_BranchHistoryEvictedWithConv(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	jsonlPath := filepath.Join(tmpDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, []byte(jsonlWithBranchHops), 0o600))

	_, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err)
	require.NotEmpty(t, branchSet(t, sessionID), "history populated on first scan")

	require.NoError(t, os.Remove(jsonlPath))
	_, err = LoadSessionsIndex(tmpDir)
	require.NoError(t, err)
	assert.Empty(t, branchSet(t, sessionID), "history evicted with the vanished conv")
}
