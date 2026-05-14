package conv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunMv_PreservesAllFields(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()

	srcRealPath := filepath.Join(tmpDir, "src-project")
	dstRealPath := filepath.Join(tmpDir, "dst-project")

	require.NoError(t, os.MkdirAll(srcRealPath, 0755))
	require.NoError(t, os.MkdirAll(dstRealPath, 0755))

	srcProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(srcRealPath))
	dstProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(dstRealPath))

	require.NoError(t, os.MkdirAll(srcProjectDir, 0755))
	require.NoError(t, os.MkdirAll(dstProjectDir, 0755))

	// Create source conversation file with all metadata
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	convContent := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + srcRealPath + `","gitBranch":"feature-branch","message":{"role":"user","content":"Test prompt for move"},"timestamp":"2024-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"OK"},"timestamp":"2024-01-01T00:00:05Z"}
{"type":"custom-title","customTitle":"Move Custom Title","sessionId":"` + sessionID + `"}
{"type":"summary","summary":"Move test summary - must be preserved","timestamp":"2024-01-01T00:05:00Z"}
`
	srcConvFile := filepath.Join(srcProjectDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(srcConvFile, []byte(convContent), 0644))

	// Load source - scans .jsonl, populates DB
	loadedIndex, err := LoadSessionsIndex(srcProjectDir)
	require.NoError(t, err, "Failed to load source index")

	found, _ := FindSessionByID(loadedIndex, sessionID)
	require.NotNil(t, found, "Source session not found")

	assert.Equal(t, "Move test summary - must be preserved", found.Summary, "Source Summary mismatch")
	assert.Equal(t, "Move Custom Title", found.CustomTitle, "Source CustomTitle mismatch")

	// Simulate move: rename file to destination
	dstConvFile := filepath.Join(dstProjectDir, sessionID+".jsonl")
	if err := os.Rename(srcConvFile, dstConvFile); err != nil {
		require.NoError(t, CopyFile(srcConvFile, dstConvFile))
		os.Remove(srcConvFile)
	}

	// Load destination - scans moved .jsonl
	loadedDstIndex, err := LoadSessionsIndex(dstProjectDir)
	require.NoError(t, err, "Failed to load destination index")

	dstEntry, _ := FindSessionByID(loadedDstIndex, sessionID)
	require.NotNil(t, dstEntry, "Destination session not found")

	// Verify fields preserved (scanned from file)
	assert.Equal(t, "Move test summary - must be preserved", dstEntry.Summary, "Summary not preserved")
	assert.Equal(t, "Move Custom Title", dstEntry.CustomTitle, "CustomTitle not preserved")
	assert.Equal(t, "Test prompt for move", dstEntry.FirstPrompt, "FirstPrompt not preserved")
	assert.Equal(t, "feature-branch", dstEntry.GitBranch, "GitBranch not preserved")

	// Verify source no longer has the entry (file was moved)
	loadedSrcIndex, _ := LoadSessionsIndex(srcProjectDir)
	assert.Empty(t, loadedSrcIndex.Entries, "expected 0 entries in source after move")
}

func TestRemoveSessionByID(t *testing.T) {
	index := &SessionsIndex{
		Version: 1,
		Entries: []SessionEntry{
			{SessionID: "aaa", FirstPrompt: "first"},
			{SessionID: "bbb", FirstPrompt: "second"},
			{SessionID: "ccc", FirstPrompt: "third"},
		},
	}

	removed := RemoveSessionByID(index, "bbb")
	assert.True(t, removed, "RemoveSessionByID should return true when entry exists")
	assert.Len(t, index.Entries, 2, "Expected 2 entries after removal")

	for _, e := range index.Entries {
		assert.NotEqual(t, "bbb", e.SessionID, "Entry 'bbb' should have been removed")
	}

	removed = RemoveSessionByID(index, "zzz")
	assert.False(t, removed, "RemoveSessionByID should return false when entry doesn't exist")
}
