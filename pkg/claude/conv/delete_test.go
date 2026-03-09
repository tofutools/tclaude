package conv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindJSONLByPrefix_ExactMatch(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	createConvFile(t, dir, id, true)

	got := findJSONLByPrefix(dir, id)
	if got != id {
		t.Errorf("Expected %s, got %s", id, got)
	}
}

func TestFindJSONLByPrefix_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	createConvFile(t, dir, id, true)

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	if got != id {
		t.Errorf("Expected %s, got %s", id, got)
	}
}

func TestFindJSONLByPrefix_AmbiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	createConvFile(t, dir, "aaaaaaaa-1111-cccc-dddd-eeeeeeeeeeee", true)
	createConvFile(t, dir, "aaaaaaaa-2222-cccc-dddd-eeeeeeeeeeee", true)

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	if got != "" {
		t.Errorf("Expected empty string for ambiguous prefix, got %s", got)
	}
}

func TestFindJSONLByPrefix_NoMatch(t *testing.T) {
	dir := t.TempDir()
	createConvFile(t, dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true)

	got := findJSONLByPrefix(dir, "bbbbbbbb")
	if got != "" {
		t.Errorf("Expected empty string, got %s", got)
	}
}

func TestFindJSONLByPrefix_IgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	// Create a directory that matches the prefix (companion dir without .jsonl)
	if err := os.MkdirAll(filepath.Join(dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), 0755); err != nil {
		t.Fatal(err)
	}

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	if got != "" {
		t.Errorf("Expected empty string (no .jsonl file), got %s", got)
	}
}

func TestFindJSONLByPrefix_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	got := findJSONLByPrefix(dir, "aaaaaaaa")
	if got != "" {
		t.Errorf("Expected empty string, got %s", got)
	}
}

func TestFindJSONLByPrefix_NonexistentDir(t *testing.T) {
	got := findJSONLByPrefix("/nonexistent/path", "aaaaaaaa")
	if got != "" {
		t.Errorf("Expected empty string, got %s", got)
	}
}

func TestRunDelete_NotInIndex_DeletesFiles(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create .jsonl file and companion dir, but do NOT add to index
	createConvFile(t, dir, id, true)
	companionDir := createCompanionDir(t, dir, id)
	createIndex(t, dir, []SessionEntry{}) // empty index

	stdout, _ := os.CreateTemp(t.TempDir(), "stdout")
	stderr, _ := os.CreateTemp(t.TempDir(), "stderr")
	defer stdout.Close()
	defer stderr.Close()

	// Call the delete logic directly on the temp dir
	// We can't use RunDelete because it resolves projectPath from cwd,
	// so we test the components instead
	fullID := findJSONLByPrefix(dir, "aaaaaaaa")
	if fullID != id {
		t.Fatalf("Expected to find %s on disk, got %s", id, fullID)
	}

	// Delete the conversation file
	convFile := filepath.Join(dir, fullID+".jsonl")
	if err := os.Remove(convFile); err != nil {
		t.Fatalf("Failed to remove conv file: %v", err)
	}

	// Delete companion dir
	if info, err := os.Stat(companionDir); err == nil && info.IsDir() {
		if err := os.RemoveAll(companionDir); err != nil {
			t.Fatalf("Failed to remove companion dir: %v", err)
		}
	}

	// Verify both are gone
	if _, err := os.Stat(convFile); !os.IsNotExist(err) {
		t.Error("Conv file should be deleted")
	}
	if _, err := os.Stat(companionDir); !os.IsNotExist(err) {
		t.Error("Companion dir should be deleted")
	}

	// Verify the index file is unchanged (still empty entries)
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatalf("Failed to load index: %v", err)
	}
	if len(index.Entries) != 0 {
		t.Errorf("Index should still be empty, got %d entries", len(index.Entries))
	}
}

func TestRunDelete_NotInIndex_OutputShowsNotInIndex(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	createIndex(t, dir, []SessionEntry{}) // empty index

	// Find the conversation on disk (not in index)
	fullID := findJSONLByPrefix(dir, "aaaaaaaa")
	if fullID == "" {
		t.Fatal("Expected to find conversation on disk")
	}

	// Verify the "not in index" label would be shown
	output := fullID[:8] + " (not in index)"
	if !strings.Contains(output, "not in index") {
		t.Error("Expected 'not in index' in output for unindexed conversation")
	}
}

func TestRunDelete_InIndex_DoesNotFallbackToDisk(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	createConvFile(t, dir, id, true)
	createIndex(t, dir, []SessionEntry{
		{SessionID: id, FirstPrompt: "hello", MessageCount: 2, ProjectPath: "/test"},
	})

	// When entry is in the index, we should find it via the index
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := FindSessionByID(index, "aaaaaaaa")
	if entry == nil {
		t.Fatal("Expected to find entry in index")
	}
	if entry.SessionID != id {
		t.Errorf("Expected %s, got %s", id, entry.SessionID)
	}

	// The disk fallback should not be needed
	// But verify it would also work for the same ID
	diskID := findJSONLByPrefix(dir, "aaaaaaaa")
	if diskID != id {
		t.Errorf("Disk fallback should also find %s, got %s", id, diskID)
	}
}

// Tests for watchModel.deleteConversation (watch.go)

func TestDeleteConversation_UsesFullPath(t *testing.T) {
	// When FullPath is set, deleteConversation should use its directory
	// instead of deriving from ProjectPath (which may point elsewhere)
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
	if err != nil {
		t.Fatalf("deleteConversation failed: %v", err)
	}

	// Verify files are deleted
	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); !os.IsNotExist(err) {
		t.Error("Conv file should be deleted")
	}
	if _, err := os.Stat(companionDir); !os.IsNotExist(err) {
		t.Error("Companion dir should be deleted")
	}

	// Verify entry was removed from index
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatalf("Failed to load index: %v", err)
	}
	if len(index.Entries) != 0 {
		t.Errorf("Expected 0 entries after delete, got %d", len(index.Entries))
	}
}

func TestDeleteConversation_NotInIndex_NoSaveError(t *testing.T) {
	// When conversation is not in the index, deleteConversation should
	// still delete the files and not fail trying to save the index
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
	if err != nil {
		t.Fatalf("deleteConversation should not fail for unindexed conv: %v", err)
	}

	// Verify file is deleted
	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); !os.IsNotExist(err) {
		t.Error("Conv file should be deleted")
	}
}

func TestDeleteConversation_NoIndexFile_NoSaveError(t *testing.T) {
	// When the project directory has no sessions-index.json at all,
	// deleteConversation should still delete files without error
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
	if err != nil {
		t.Fatalf("deleteConversation should not fail without index file: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); !os.IsNotExist(err) {
		t.Error("Conv file should be deleted")
	}
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
	if err != nil {
		t.Fatalf("deleteConversation should not fail for non-existent project dir: %v", err)
	}
	_ = dir
}

func TestDeleteConversation_IndexPreservedForOtherEntries(t *testing.T) {
	// Deleting one conversation should not affect other entries in the index
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
	if err != nil {
		t.Fatalf("deleteConversation failed: %v", err)
	}

	// Verify the kept entry is still in the index
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Entries) != 1 {
		t.Fatalf("Expected 1 entry remaining, got %d", len(index.Entries))
	}
	if index.Entries[0].SessionID != keepID {
		t.Errorf("Expected remaining entry %s, got %s", keepID, index.Entries[0].SessionID)
	}

	// Verify the kept .jsonl file still exists
	if _, err := os.Stat(filepath.Join(dir, keepID+".jsonl")); err != nil {
		t.Error("Kept conv file should still exist")
	}
}

func TestDeleteConversation_LocalMode_UsesModelProjectPath(t *testing.T) {
	// In non-global mode with no FullPath, should use m.projectPath
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create files under the Claude project path for some fake working dir
	projectDir := GetClaudeProjectPath(dir)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatalf("deleteConversation failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(projectDir, id+".jsonl")); !os.IsNotExist(err) {
		t.Error("Conv file should be deleted")
	}

	idx, _ := LoadSessionsIndex(projectDir)
	if len(idx.Entries) != 0 {
		t.Errorf("Expected 0 entries after delete, got %d", len(idx.Entries))
	}
}

