package conv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRunCp_PreservesAllFields(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()

	srcRealPath := filepath.Join(tmpDir, "src-project")
	dstRealPath := filepath.Join(tmpDir, "dst-project")

	require.NoError(t, os.MkdirAll(srcRealPath, 0755))
	require.NoError(t, os.MkdirAll(dstRealPath, 0755))

	srcProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(srcRealPath))
	dstProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(dstRealPath))

	require.NoError(t, os.MkdirAll(srcProjectDir, 0755))

	// Create source conversation file with all metadata inline
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	convContent := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + srcRealPath + `","gitBranch":"main","message":{"role":"user","content":"Test prompt"},"timestamp":"2024-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"Hello"},"timestamp":"2024-01-01T00:00:05Z"}
{"type":"custom-title","customTitle":"Custom Title Here","sessionId":"` + sessionID + `"}
{"type":"summary","summary":"Test summary that should be preserved","timestamp":"2024-01-01T00:05:00Z"}
`
	require.NoError(t, os.WriteFile(filepath.Join(srcProjectDir, sessionID+".jsonl"), []byte(convContent), 0644))

	// Load source - scans .jsonl and populates DB
	loadedIndex, err := LoadSessionsIndex(srcProjectDir)
	require.NoError(t, err, "Failed to load source index")

	found, _ := FindSessionByID(loadedIndex, sessionID)
	require.NotNil(t, found, "Source session not found")

	// Verify source has all fields from scanning
	assert.Equal(t, "Test summary that should be preserved", found.Summary, "Source Summary mismatch")
	assert.Equal(t, "Custom Title Here", found.CustomTitle, "Source CustomTitle mismatch")
	assert.Equal(t, "main", found.GitBranch, "Source GitBranch mismatch")

	// Simulate copy: create destination with new UUID
	newConvID := "11111111-2222-3333-4444-555555555555"
	require.NoError(t, os.MkdirAll(dstProjectDir, 0755))
	srcData, _ := os.ReadFile(filepath.Join(srcProjectDir, sessionID+".jsonl"))
	dstData := strings.ReplaceAll(string(srcData), sessionID, newConvID)
	dstConvFile := filepath.Join(dstProjectDir, newConvID+".jsonl")
	require.NoError(t, os.WriteFile(dstConvFile, []byte(dstData), 0644))

	// Load destination - scans .jsonl and populates DB
	loadedDstIndex, err := LoadSessionsIndex(dstProjectDir)
	require.NoError(t, err, "Failed to load destination index")

	dstEntry, _ := FindSessionByID(loadedDstIndex, newConvID)
	require.NotNil(t, dstEntry, "Destination session not found")

	// Verify all fields are preserved
	assert.Equal(t, "Test summary that should be preserved", dstEntry.Summary, "Summary not preserved")
	assert.Equal(t, "Custom Title Here", dstEntry.CustomTitle, "CustomTitle not preserved")
	assert.Equal(t, "Test prompt", dstEntry.FirstPrompt, "FirstPrompt not preserved")
	assert.Equal(t, "main", dstEntry.GitBranch, "GitBranch not preserved")

	// Verify DB was populated for destination
	row, err := db.GetConvIndex(newConvID)
	require.NoError(t, err)
	require.NotNil(t, row, "expected DB entry for destination conv")
	assert.Equal(t, "Test summary that should be preserved", row.Summary, "DB Summary mismatch")
}

func TestCopyConversationFile_ReplacesSessionID(t *testing.T) {
	tmpDir := t.TempDir()

	oldID := "old-session-id-0000000000000000000"
	newID := "new-session-id-1111111111111111111"

	srcFile := filepath.Join(tmpDir, "src.jsonl")
	dstFile := filepath.Join(tmpDir, "dst.jsonl")

	content := `{"sessionId":"old-session-id-0000000000000000000","type":"user"}
{"sessionId":"old-session-id-0000000000000000000","type":"assistant"}
`
	require.NoError(t, os.WriteFile(srcFile, []byte(content), 0644))

	require.NoError(t, CopyConversationFile(srcFile, dstFile, oldID, newID), "CopyConversationFile failed")

	data, err := os.ReadFile(dstFile)
	require.NoError(t, err)

	assert.NotContains(t, string(data), oldID, "Old session ID still present in copied file")
	assert.Contains(t, string(data), newID, "New session ID not found in copied file")
}
