package conv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunMv_PreservesAllFields(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()

	srcRealPath := filepath.Join(tmpDir, "src-project")
	dstRealPath := filepath.Join(tmpDir, "dst-project")

	if err := os.MkdirAll(srcRealPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstRealPath, 0755); err != nil {
		t.Fatal(err)
	}

	srcProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(srcRealPath))
	dstProjectDir := filepath.Join(tmpDir, "claude-projects", PathToProjectDir(dstRealPath))

	if err := os.MkdirAll(srcProjectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create source conversation file with all metadata
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	convContent := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + srcRealPath + `","gitBranch":"feature-branch","message":{"role":"user","content":"Test prompt for move"},"timestamp":"2024-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"OK"},"timestamp":"2024-01-01T00:00:05Z"}
{"type":"custom-title","customTitle":"Move Custom Title","sessionId":"` + sessionID + `"}
{"type":"summary","summary":"Move test summary - must be preserved","timestamp":"2024-01-01T00:05:00Z"}
`
	srcConvFile := filepath.Join(srcProjectDir, sessionID+".jsonl")
	if err := os.WriteFile(srcConvFile, []byte(convContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Load source - scans .jsonl, populates DB
	loadedIndex, err := LoadSessionsIndex(srcProjectDir)
	if err != nil {
		t.Fatalf("Failed to load source index: %v", err)
	}

	found, _ := FindSessionByID(loadedIndex, sessionID)
	if found == nil {
		t.Fatal("Source session not found")
	}

	if found.Summary != "Move test summary - must be preserved" {
		t.Errorf("Source Summary mismatch: got %q", found.Summary)
	}
	if found.CustomTitle != "Move Custom Title" {
		t.Errorf("Source CustomTitle mismatch: got %q", found.CustomTitle)
	}

	// Simulate move: rename file to destination
	dstConvFile := filepath.Join(dstProjectDir, sessionID+".jsonl")
	if err := os.Rename(srcConvFile, dstConvFile); err != nil {
		if err := CopyFile(srcConvFile, dstConvFile); err != nil {
			t.Fatal(err)
		}
		os.Remove(srcConvFile)
	}

	// Load destination - scans moved .jsonl
	loadedDstIndex, err := LoadSessionsIndex(dstProjectDir)
	if err != nil {
		t.Fatalf("Failed to load destination index: %v", err)
	}

	dstEntry, _ := FindSessionByID(loadedDstIndex, sessionID)
	if dstEntry == nil {
		t.Fatal("Destination session not found")
	}

	// Verify fields preserved (scanned from file)
	if dstEntry.Summary != "Move test summary - must be preserved" {
		t.Errorf("Summary not preserved: got %q", dstEntry.Summary)
	}
	if dstEntry.CustomTitle != "Move Custom Title" {
		t.Errorf("CustomTitle not preserved: got %q", dstEntry.CustomTitle)
	}
	if dstEntry.FirstPrompt != "Test prompt for move" {
		t.Errorf("FirstPrompt not preserved: got %q", dstEntry.FirstPrompt)
	}
	if dstEntry.GitBranch != "feature-branch" {
		t.Errorf("GitBranch not preserved: got %q", dstEntry.GitBranch)
	}

	// Verify source no longer has the entry (file was moved)
	loadedSrcIndex, _ := LoadSessionsIndex(srcProjectDir)
	if len(loadedSrcIndex.Entries) != 0 {
		t.Errorf("expected 0 entries in source after move, got %d", len(loadedSrcIndex.Entries))
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

	removed := RemoveSessionByID(index, "bbb")
	if !removed {
		t.Error("RemoveSessionByID should return true when entry exists")
	}
	if len(index.Entries) != 2 {
		t.Errorf("Expected 2 entries after removal, got %d", len(index.Entries))
	}

	for _, e := range index.Entries {
		if e.SessionID == "bbb" {
			t.Error("Entry 'bbb' should have been removed")
		}
	}

	removed = RemoveSessionByID(index, "zzz")
	if removed {
		t.Error("RemoveSessionByID should return false when entry doesn't exist")
	}
}
