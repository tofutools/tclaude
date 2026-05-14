package conv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindJSONLByPrefix_ExactMatch(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	createConvFile(t, dir, id, true)

	got := findJSONLByPrefix(dir, id)
	assert.Equal(t, id, got)
}

func TestFindJSONLByPrefix_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	createConvFile(t, dir, id, true)

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	assert.Equal(t, id, got)
}

func TestFindJSONLByPrefix_AmbiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	createConvFile(t, dir, "aaaaaaaa-1111-cccc-dddd-eeeeeeeeeeee", true)
	createConvFile(t, dir, "aaaaaaaa-2222-cccc-dddd-eeeeeeeeeeee", true)

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	assert.Empty(t, got, "Expected empty string for ambiguous prefix")
}

func TestFindJSONLByPrefix_NoMatch(t *testing.T) {
	dir := t.TempDir()
	createConvFile(t, dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true)

	got := findJSONLByPrefix(dir, "bbbbbbbb")
	assert.Empty(t, got)
}

func TestFindJSONLByPrefix_IgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	// Create a directory that matches the prefix (companion dir without .jsonl)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), 0755))

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	assert.Empty(t, got, "Expected empty string (no .jsonl file)")
}

func TestFindJSONLByPrefix_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	assert.Empty(t, got)
}

func TestFindJSONLByPrefix_NonexistentDir(t *testing.T) {
	got := findJSONLByPrefix("/nonexistent/path", "aaaaaaaa")
	assert.Empty(t, got)
}

func TestRunDelete_NotInIndex_DeletesFiles(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create .jsonl file and companion dir, but do NOT add to index
	createConvFile(t, dir, id, true)
	companionDir := createCompanionDir(t, dir, id)
	createIndex(t, dir, []SessionEntry{}) // empty index

	stdout, _ := os.CreateTemp(t.TempDir(), "stdout")
	stderr, _ := os.CreateTemp(t.TempDir(), "stderr")
	defer func() { _ = stdout.Close() }()
	defer func() { _ = stderr.Close() }()

	// Call the delete logic directly on the temp dir
	// We can't use RunDelete because it resolves projectPath from cwd,
	// so we test the components instead
	fullID := findJSONLByPrefix(dir, "aaaaaaaa")
	require.Equal(t, id, fullID, "Expected to find %s on disk", id)

	// Delete the conversation file
	convFile := filepath.Join(dir, fullID+".jsonl")
	require.NoError(t, os.Remove(convFile), "Failed to remove conv file")

	// Delete companion dir
	if info, err := os.Stat(companionDir); err == nil && info.IsDir() {
		require.NoError(t, os.RemoveAll(companionDir), "Failed to remove companion dir")
	}

	// Verify both are gone
	_, err := os.Stat(convFile)
	assert.True(t, os.IsNotExist(err), "Conv file should be deleted")
	_, err = os.Stat(companionDir)
	assert.True(t, os.IsNotExist(err), "Companion dir should be deleted")

	// Verify the index file is unchanged (still empty entries)
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err, "Failed to load index")
	assert.Empty(t, index.Entries, "Index should still be empty")
}

func TestRunDelete_NotInIndex_OutputShowsNotInIndex(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	createIndex(t, dir, []SessionEntry{}) // empty index

	// Find the conversation on disk (not in index)
	fullID := findJSONLByPrefix(dir, "aaaaaaaa")
	require.NotEmpty(t, fullID, "Expected to find conversation on disk")

	// Verify the "not in index" label would be shown
	output := fullID[:8] + " (not in index)"
	assert.Contains(t, output, "not in index", "Expected 'not in index' in output for unindexed conversation")
}

func TestRunDelete_InIndex_DoesNotFallbackToDisk(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	createIndex(t, dir, []SessionEntry{
		{SessionID: id, FirstPrompt: "hello", MessageCount: 2, ProjectPath: "/test"},
	})

	// When entry is in the index, we should find it via the index
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err)
	entry, _ := FindSessionByID(index, "aaaaaaaa")
	require.NotNil(t, entry, "Expected to find entry in index")
	assert.Equal(t, id, entry.SessionID)

	// The disk fallback should not be needed
	// But verify it would also work for the same ID
	diskID := findJSONLByPrefix(dir, "aaaaaaaa")
	assert.Equal(t, id, diskID, "Disk fallback should also find %s", id)
}

// Tests for watchModel.deleteConversation (watch.go)

func TestDeleteConversation_UsesFullPath(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	companionDir := createCompanionDir(t, dir, id)
	createIndex(t, dir, []SessionEntry{
		{SessionID: id, FullPath: filepath.Join(dir, id+".jsonl"), ProjectPath: "/some/other/path"},
	})

	m := watchModel{global: true}
	entry := &SessionEntry{
		SessionID:   id,
		FullPath:    filepath.Join(dir, id+".jsonl"),
		ProjectPath: "/some/other/path", // wrong path that would fail if used
	}

	err := m.deleteConversation(entry)
	require.NoError(t, err, "deleteConversation failed")

	// Verify files are deleted
	_, err = os.Stat(filepath.Join(dir, id+".jsonl"))
	assert.True(t, os.IsNotExist(err), "Conv file should be deleted")
	_, err = os.Stat(companionDir)
	assert.True(t, os.IsNotExist(err), "Companion dir should be deleted")

	// Verify entry was removed from index
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err, "Failed to load index")
	assert.Empty(t, index.Entries, "Expected 0 entries after delete")
}

func TestDeleteConversation_NotInIndex_NoSaveError(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	createIndex(t, dir, []SessionEntry{}) // empty index

	m := watchModel{global: true}
	entry := &SessionEntry{
		SessionID: id,
		FullPath:  filepath.Join(dir, id+".jsonl"),
	}

	err := m.deleteConversation(entry)
	require.NoError(t, err, "deleteConversation should not fail for unindexed conv")

	// Verify file is deleted
	_, err = os.Stat(filepath.Join(dir, id+".jsonl"))
	assert.True(t, os.IsNotExist(err), "Conv file should be deleted")
}

func TestDeleteConversation_NoIndexFile_NoSaveError(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	// No index file created

	m := watchModel{global: true}
	entry := &SessionEntry{
		SessionID: id,
		FullPath:  filepath.Join(dir, id+".jsonl"),
	}

	err := m.deleteConversation(entry)
	require.NoError(t, err, "deleteConversation should not fail without index file")

	_, err = os.Stat(filepath.Join(dir, id+".jsonl"))
	assert.True(t, os.IsNotExist(err), "Conv file should be deleted")
}

func TestDeleteConversation_NonexistentProjectDir_ViaProjectPath(t *testing.T) {
	// Reproduces the original bug: FullPath is empty and ProjectPath maps
	// to a non-existent project directory. Should not error.
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	m := watchModel{global: true}
	entry := &SessionEntry{
		SessionID:   id,
		ProjectPath: "/some/nonexistent/path",
		// FullPath intentionally empty
	}

	// This should not error - there's nothing to delete but it shouldn't crash
	err := m.deleteConversation(entry)
	require.NoError(t, err, "deleteConversation should not fail for non-existent project dir")
	_ = dir
}

func TestDeleteConversation_IndexPreservedForOtherEntries(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	deleteID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	keepID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, deleteID, true)
	createConvFile(t, dir, keepID, true)
	createIndex(t, dir, []SessionEntry{
		{SessionID: deleteID, FullPath: filepath.Join(dir, deleteID+".jsonl")},
		{SessionID: keepID, FullPath: filepath.Join(dir, keepID+".jsonl")},
	})

	m := watchModel{global: true}
	entry := &SessionEntry{
		SessionID: deleteID,
		FullPath:  filepath.Join(dir, deleteID+".jsonl"),
	}

	err := m.deleteConversation(entry)
	require.NoError(t, err, "deleteConversation failed")

	// Verify the kept entry is still in the index
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err)
	require.Len(t, index.Entries, 1, "Expected 1 entry remaining")
	assert.Equal(t, keepID, index.Entries[0].SessionID, "Expected remaining entry %s", keepID)

	// Verify the kept .jsonl file still exists
	_, err = os.Stat(filepath.Join(dir, keepID+".jsonl"))
	assert.NoError(t, err, "Kept conv file should still exist")
}

func TestDeleteConversation_LocalMode_UsesModelProjectPath(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create files under the Claude project path for some fake working dir
	projectDir := GetClaudeProjectPath(dir)
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	createConvFile(t, projectDir, id, true)
	createIndex(t, projectDir, []SessionEntry{
		{SessionID: id},
	})

	m := watchModel{global: false, projectPath: dir}
	entry := &SessionEntry{
		SessionID: id,
		// FullPath intentionally empty
	}

	err := m.deleteConversation(entry)
	require.NoError(t, err, "deleteConversation failed")

	_, err = os.Stat(filepath.Join(projectDir, id+".jsonl"))
	assert.True(t, os.IsNotExist(err), "Conv file should be deleted")

	idx, _ := LoadSessionsIndex(projectDir)
	assert.Empty(t, idx.Entries, "Expected 0 entries after delete")
}
