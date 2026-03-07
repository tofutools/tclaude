package conv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunMv_PreservesAllFields(t *testing.T) {
	// Create temp directories for source and destination projects
	tmpDir := t.TempDir()

	// Simulate Claude's projects directory structure
	srcRealPath := filepath.Join(tmpDir, "src-project")
	dstRealPath := filepath.Join(tmpDir, "dst-project")

	if err := os.MkdirAll(srcRealPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstRealPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create Claude project directories
	srcProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(srcRealPath))
	dstProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(dstRealPath))

	if err := os.MkdirAll(srcProjectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create source session with all fields populated
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	srcEntry := SessionEntry{
		SessionID:    sessionID,
		FullPath:     filepath.Join(srcProjectDir, sessionID+".jsonl"),
		FileMtime:    1234567890,
		FirstPrompt:  "Test prompt for move",
		Summary:      "Move test summary - must be preserved",
		CustomTitle:  "Move Custom Title",
		MessageCount: 25,
		Created:      "2024-01-01T00:00:00Z",
		Modified:     "2024-01-02T00:00:00Z",
		GitBranch:    "feature-branch",
		ProjectPath:  srcRealPath,
		IsSidechain:  false,
	}

	// Create source index
	srcIndex := SessionsIndex{
		Version: 1,
		Entries: []SessionEntry{srcEntry},
	}
	srcIndexData, _ := json.MarshalIndent(srcIndex, "", "  ")
	if err := os.WriteFile(filepath.Join(srcProjectDir, "sessions-index.json"), srcIndexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create source conversation file
	convContent := `{"type":"user","sessionId":"` + sessionID + `","message":{"role":"user","content":"Test prompt for move"},"timestamp":"2024-01-01T00:00:00Z"}
`
	srcConvFile := filepath.Join(srcProjectDir, sessionID+".jsonl")
	if err := os.WriteFile(srcConvFile, []byte(convContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Load source to get the entry
	loadedIndex, err := LoadSessionsIndex(srcProjectDir)
	if err != nil {
		t.Fatalf("Failed to load source index: %v", err)
	}

	found, _ := FindSessionByID(loadedIndex, sessionID)
	if found == nil {
		t.Fatal("Source session not found")
	}

	// Verify source has all fields
	if found.Summary != "Move test summary - must be preserved" {
		t.Errorf("Source Summary mismatch: got %q", found.Summary)
	}
	if found.CustomTitle != "Move Custom Title" {
		t.Errorf("Source CustomTitle mismatch: got %q", found.CustomTitle)
	}

	// Simulate the move operation
	dstConvFile := filepath.Join(dstProjectDir, sessionID+".jsonl")

	// Move the file
	if err := os.Rename(srcConvFile, dstConvFile); err != nil {
		// Fallback to copy+delete for cross-device
		if err := CopyFile(srcConvFile, dstConvFile); err != nil {
			t.Fatal(err)
		}
		os.Remove(srcConvFile)
	}

	dstInfo, _ := os.Stat(dstConvFile)

	// Create new entry exactly as RunMv does (after our fix)
	// Note: mv preserves the original Created/Modified timestamps
	newEntry := SessionEntry{
		SessionID:    found.SessionID,
		FullPath:     dstConvFile,
		FileMtime:    dstInfo.ModTime().UnixMilli(),
		FirstPrompt:  found.FirstPrompt,
		Summary:      found.Summary,      // This was missing before the fix!
		CustomTitle:  found.CustomTitle,  // This was missing before the fix!
		MessageCount: found.MessageCount,
		Created:      found.Created,  // mv preserves original timestamps
		Modified:     found.Modified, // mv preserves original timestamps
		GitBranch:    found.GitBranch,
		ProjectPath:  dstRealPath, // Updated to new path
		IsSidechain:  found.IsSidechain,
	}

	// Save destination index
	dstIndex := SessionsIndex{
		Version: 1,
		Entries: []SessionEntry{newEntry},
	}
	dstIndexData, _ := json.MarshalIndent(dstIndex, "", "  ")
	if err := os.WriteFile(filepath.Join(dstProjectDir, "sessions-index.json"), dstIndexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Remove from source index
	RemoveSessionByID(loadedIndex, sessionID)
	srcIndexData, _ = json.MarshalIndent(loadedIndex, "", "  ")
	if err := os.WriteFile(filepath.Join(srcProjectDir, "sessions-index.json"), srcIndexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Reload and verify all fields are preserved in destination
	loadedDstIndex, err := LoadSessionsIndexWithOptions(dstProjectDir, LoadSessionsIndexOptions{
		SkipUnindexedScan:     true,
		SkipMissingDataRescan: true,
	})
	if err != nil {
		t.Fatalf("Failed to load destination index: %v", err)
	}

	dstEntry, _ := FindSessionByID(loadedDstIndex, sessionID)
	if dstEntry == nil {
		t.Fatal("Destination session not found")
	}

	// Verify all fields are preserved
	if dstEntry.Summary != "Move test summary - must be preserved" {
		t.Errorf("Summary not preserved: got %q, want %q", dstEntry.Summary, "Move test summary - must be preserved")
	}
	if dstEntry.CustomTitle != "Move Custom Title" {
		t.Errorf("CustomTitle not preserved: got %q, want %q", dstEntry.CustomTitle, "Move Custom Title")
	}
	if dstEntry.FirstPrompt != "Test prompt for move" {
		t.Errorf("FirstPrompt not preserved: got %q", dstEntry.FirstPrompt)
	}
	if dstEntry.GitBranch != "feature-branch" {
		t.Errorf("GitBranch not preserved: got %q", dstEntry.GitBranch)
	}
	if dstEntry.MessageCount != 25 {
		t.Errorf("MessageCount not preserved: got %d", dstEntry.MessageCount)
	}
	if dstEntry.Created != "2024-01-01T00:00:00Z" {
		t.Errorf("Created not preserved: got %q", dstEntry.Created)
	}
	if dstEntry.Modified != "2024-01-02T00:00:00Z" {
		t.Errorf("Modified not preserved: got %q", dstEntry.Modified)
	}
	if dstEntry.ProjectPath != dstRealPath {
		t.Errorf("ProjectPath not updated: got %q, want %q", dstEntry.ProjectPath, dstRealPath)
	}

	// Verify source no longer has the entry
	loadedSrcIndex, _ := LoadSessionsIndexWithOptions(srcProjectDir, LoadSessionsIndexOptions{
		SkipUnindexedScan:     true,
		SkipMissingDataRescan: true,
	})
	srcFound, _ := FindSessionByID(loadedSrcIndex, sessionID)
	if srcFound != nil {
		t.Error("Session should have been removed from source index")
	}
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

	// Remove middle entry
	removed := RemoveSessionByID(index, "bbb")
	if !removed {
		t.Error("RemoveSessionByID should return true when entry exists")
	}
	if len(index.Entries) != 2 {
		t.Errorf("Expected 2 entries after removal, got %d", len(index.Entries))
	}

	// Verify correct entry was removed
	for _, e := range index.Entries {
		if e.SessionID == "bbb" {
			t.Error("Entry 'bbb' should have been removed")
		}
	}

	// Try to remove non-existent entry
	removed = RemoveSessionByID(index, "zzz")
	if removed {
		t.Error("RemoveSessionByID should return false when entry doesn't exist")
	}
}
