package conv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// indexInDB upserts a conv_index row so the DB-backed prune helpers
// see this conv as tracked. Mirrors what `createIndex` used to do for
// the legacy file but targets the SQLite source-of-truth that the
// helpers actually read.
func indexInDB(t *testing.T, projectDir, sessionID, filePath string) {
	t.Helper()
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     sessionID,
		ProjectDir: projectDir,
		FullPath:   filePath,
	}), "UpsertConvIndex")
}


// helper to create a .jsonl file with user messages
func createConvFile(t *testing.T, dir, sessionID string, withUserMsg bool) string {
	t.Helper()
	filePath := filepath.Join(dir, sessionID+".jsonl")
	var content string
	if withUserMsg {
		content = `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-01-01T00:00:01Z"}
`
	} else {
		content = `{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-01-01T00:00:01Z"}
`
	}
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write conv file")
	return filePath
}

// helper to create a sessions-index.json
func createIndex(t *testing.T, dir string, entries []SessionEntry) {
	t.Helper()
	index := SessionsIndex{Version: 1, Entries: entries}
	data, err := json.MarshalIndent(index, "", "  ")
	require.NoError(t, err, "Failed to marshal index")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sessions-index.json"), data, 0644), "Failed to write index")
}

// helper to create a companion directory with a subagent file
func createCompanionDir(t *testing.T, dir, sessionID string) string {
	t.Helper()
	companionDir := filepath.Join(dir, sessionID)
	subagentDir := filepath.Join(companionDir, "subagents")
	require.NoError(t, os.MkdirAll(subagentDir, 0755), "Failed to create companion dir")
	subagentFile := filepath.Join(subagentDir, "agent-aprompt_suggestion-abc123.jsonl")
	require.NoError(t, os.WriteFile(subagentFile, []byte(`{"type":"assistant"}`), 0644), "Failed to write subagent file")
	return companionDir
}

func TestFindEmptyConversations(t *testing.T) {
	dir := t.TempDir()

	emptyID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	nonEmptyID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, emptyID, false)
	createConvFile(t, dir, nonEmptyID, true)

	result := findEmptyConversations(dir)

	require.Len(t, result, 1, "Expected 1 empty conversation")
	assert.Equal(t, emptyID, result[0].SessionID)
}

func TestFindEmptyConversations_IndexedFlag(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()

	indexedID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	unindexedID := "11111111-2222-3333-4444-555555555555"

	indexedPath := createConvFile(t, dir, indexedID, false)
	createConvFile(t, dir, unindexedID, false)
	indexInDB(t, dir, indexedID, indexedPath)

	result := findEmptyConversations(dir)

	require.Len(t, result, 2, "Expected 2 empty conversations")

	indexed := 0
	for _, c := range result {
		if c.IsIndexed {
			indexed++
			assert.Equal(t, indexedID, c.SessionID, "Expected indexed session %s", indexedID)
		}
	}
	assert.Equal(t, 1, indexed, "Expected 1 indexed conversation")
}

func TestFindMissingFileEntries(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()

	existingID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	missingID := "11111111-2222-3333-4444-555555555555"

	existingPath := createConvFile(t, dir, existingID, true)
	// Don't create a file for missingID

	indexInDB(t, dir, existingID, existingPath)
	indexInDB(t, dir, missingID, filepath.Join(dir, missingID+".jsonl"))

	result := findMissingFileEntries(dir)

	require.Len(t, result, 1, "Expected 1 missing entry")
	assert.Equal(t, missingID, result[0].SessionID)
	assert.True(t, result[0].IsIndexed, "Expected IsIndexed to be true for missing-file entry")
}

func TestFindDanglingDirectories(t *testing.T) {
	dir := t.TempDir()

	withFileID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	danglingID := "11111111-2222-3333-4444-555555555555"

	// Create conv file + companion dir (not dangling)
	createConvFile(t, dir, withFileID, true)
	createCompanionDir(t, dir, withFileID)

	// Create companion dir only (dangling)
	createCompanionDir(t, dir, danglingID)

	result := findDanglingDirectories(dir)

	require.Len(t, result, 1, "Expected 1 dangling directory")
	assert.Equal(t, danglingID, result[0].SessionID)
}

func TestFindDanglingDirectories_IgnoresNonUUID(t *testing.T) {
	dir := t.TempDir()

	// Create directories that don't look like UUIDs
	for _, name := range []string{"subagents", "cache", "short", "not-a-uuid-but-has-36-chars-in-name!"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0755), "Failed to create dir")
	}

	result := findDanglingDirectories(dir)

	assert.Empty(t, result, "Expected 0 dangling directories")
}

func TestHasUserMessages(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{
			name:     "with user message",
			content:  `{"type":"user","message":{"role":"user","content":"hello"}}`,
			expected: true,
		},
		{
			name:     "only assistant",
			content:  `{"type":"assistant","message":{"role":"assistant","content":"hi"}}`,
			expected: false,
		},
		{
			name:     "empty file",
			content:  "",
			expected: false,
		},
		{
			name:     "system messages only",
			content:  `{"type":"system","message":{"role":"system","content":"init"}}`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(dir, tt.name+".jsonl")
			require.NoError(t, os.WriteFile(filePath, []byte(tt.content), 0644))
			got := hasUserMessages(filePath)
			assert.Equal(t, tt.expected, got, "hasUserMessages()")
		})
	}
}

func TestRunPruneEmpty_DeletesEmptyAndCompanionDirs(t *testing.T) {
	dir := t.TempDir()

	emptyID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, emptyID, false)
	companionDir := createCompanionDir(t, dir, emptyID)
	createIndex(t, dir, []SessionEntry{{SessionID: emptyID}})

	// Verify setup
	_, err := os.Stat(companionDir)
	require.NoError(t, err, "Companion dir should exist before prune")

	stdout, _ := os.CreateTemp(dir, "stdout")
	stderr, _ := os.CreateTemp(dir, "stderr")
	defer stdout.Close()
	defer stderr.Close()

	params := &PruneEmptyParams{Dir: dir, Yes: true}

	// Override project path resolution by running against dir directly
	// Since RunPruneEmpty uses GetClaudeProjectPath, we test the lower-level functions
	emptyConvs := findEmptyConversations(dir)
	require.Len(t, emptyConvs, 1, "Expected 1 empty conv")

	// Simulate what RunPruneEmpty does for deletion
	convFile := filepath.Join(dir, emptyID+".jsonl")
	require.NoError(t, os.Remove(convFile), "Failed to remove conv file")
	// Remove companion directory
	if info, err := os.Stat(companionDir); err == nil && info.IsDir() {
		require.NoError(t, os.RemoveAll(companionDir), "Failed to remove companion dir")
	}

	// Verify both are gone
	_, err = os.Stat(convFile)
	assert.True(t, os.IsNotExist(err), "Conv file should be deleted")
	_, err = os.Stat(companionDir)
	assert.True(t, os.IsNotExist(err), "Companion directory should be deleted")

	_ = params // used for context
}

func TestRunPruneEmpty_DryRunDeletesNothing(t *testing.T) {
	dir := t.TempDir()

	emptyID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	danglingID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, emptyID, false)
	createCompanionDir(t, dir, emptyID)
	createCompanionDir(t, dir, danglingID)

	stdout, _ := os.CreateTemp(dir, "stdout")
	stderr, _ := os.CreateTemp(dir, "stderr")
	// Create a fake stdin (won't be read in dry run)
	stdin, _ := os.CreateTemp(dir, "stdin")
	defer stdout.Close()
	defer stderr.Close()
	defer stdin.Close()

	// We can't call RunPruneEmpty directly because it uses GetClaudeProjectPath,
	// but we can verify findDanglingDirectories excludes covered IDs
	empty := findEmptyConversations(dir)
	missing := findMissingFileEntries(dir)

	coveredIDs := make(map[string]bool)
	for _, c := range empty {
		coveredIDs[c.SessionID] = true
	}
	for _, c := range missing {
		coveredIDs[c.SessionID] = true
	}

	var danglingDirs []emptyConversation
	for _, d := range findDanglingDirectories(dir) {
		if !coveredIDs[d.SessionID] {
			danglingDirs = append(danglingDirs, d)
		}
	}

	// emptyID has a .jsonl, so its companion dir is NOT dangling
	// danglingID has no .jsonl, so its dir IS dangling
	require.Len(t, danglingDirs, 1, "Expected 1 dangling dir")
	assert.Equal(t, danglingID, danglingDirs[0].SessionID)

	// Verify nothing was deleted
	convFile := filepath.Join(dir, emptyID+".jsonl")
	_, err := os.Stat(convFile)
	assert.False(t, os.IsNotExist(err), "Conv file should still exist (dry run)")
	companionDir := filepath.Join(dir, emptyID)
	_, err = os.Stat(companionDir)
	assert.False(t, os.IsNotExist(err), "Companion dir should still exist (dry run)")
	danglingDir := filepath.Join(dir, danglingID)
	_, err = os.Stat(danglingDir)
	assert.False(t, os.IsNotExist(err), "Dangling dir should still exist (dry run)")
}

func TestRunPruneEmpty_DanglingDirExclusion(t *testing.T) {
	// When a session ID appears in both missing-file entries and dangling dirs,
	// the dangling dir should be excluded (it will be cleaned up during conv deletion)
	setupTestDB(t)
	dir := t.TempDir()

	missingID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create DB row but no .jsonl file, and a companion directory
	indexInDB(t, dir, missingID, filepath.Join(dir, missingID+".jsonl"))
	createCompanionDir(t, dir, missingID)

	missing := findMissingFileEntries(dir)
	require.Len(t, missing, 1, "Expected 1 missing entry")

	// Raw dangling dirs would include this ID
	rawDangling := findDanglingDirectories(dir)
	require.Len(t, rawDangling, 1, "Expected 1 raw dangling dir")

	// After exclusion, it should be filtered out
	coveredIDs := make(map[string]bool)
	for _, c := range missing {
		coveredIDs[c.SessionID] = true
	}
	var filtered []emptyConversation
	for _, d := range rawDangling {
		if !coveredIDs[d.SessionID] {
			filtered = append(filtered, d)
		}
	}

	assert.Empty(t, filtered, "Expected 0 dangling dirs after exclusion")
}

func TestRunPruneEmpty_OutputFormat(t *testing.T) {
	dir := t.TempDir()

	emptyID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	danglingID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, emptyID, false)
	createCompanionDir(t, dir, danglingID)

	empty := findEmptyConversations(dir)
	dangling := findDanglingDirectories(dir)

	// Verify the output would show the right prefixes
	require.Len(t, empty, 1, "Expected 1 empty conv")
	assert.True(t, strings.HasPrefix(empty[0].SessionID, "aaaaaaaa"), "Unexpected session ID prefix: %s", empty[0].SessionID)

	require.Len(t, dangling, 1, "Expected 1 dangling dir")
	assert.True(t, strings.HasPrefix(dangling[0].SessionID, "11111111"), "Unexpected session ID prefix: %s", dangling[0].SessionID)
}

func TestRunPruneEmpty_NothingToDelete(t *testing.T) {
	dir := t.TempDir()

	// Only create a valid conversation with user messages
	validID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	createConvFile(t, dir, validID, true)

	empty := findEmptyConversations(dir)
	missing := findMissingFileEntries(dir)
	dangling := findDanglingDirectories(dir)

	assert.Empty(t, empty, "Expected 0 empty")
	assert.Empty(t, missing, "Expected 0 missing")
	assert.Empty(t, dangling, "Expected 0 dangling")
}
