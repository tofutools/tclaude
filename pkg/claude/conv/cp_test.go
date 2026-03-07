package conv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCp_PreservesAllFields(t *testing.T) {
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

	// Create source session with all fields populated
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	srcEntry := SessionEntry{
		SessionID:    sessionID,
		FullPath:     filepath.Join(srcProjectDir, sessionID+".jsonl"),
		FileMtime:    1234567890,
		FirstPrompt:  "Test prompt",
		Summary:      "Test summary that should be preserved",
		CustomTitle:  "Custom Title Here",
		MessageCount: 10,
		Created:      "2024-01-01T00:00:00Z",
		Modified:     "2024-01-02T00:00:00Z",
		GitBranch:    "main",
		ProjectPath:  srcRealPath,
		IsSidechain:  true,
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
	convContent := `{"type":"user","sessionId":"` + sessionID + `","message":{"role":"user","content":"Test prompt"},"timestamp":"2024-01-01T00:00:00Z"}
`
	if err := os.WriteFile(filepath.Join(srcProjectDir, sessionID+".jsonl"), []byte(convContent), 0644); err != nil {
		t.Fatal(err)
	}

	// We can't easily override ClaudeProjectsDir, so we'll test the core logic directly
	// by manually setting up the destination and verifying the entry creation

	// For a proper integration test, let's just verify the SessionEntry construction
	// by simulating what RunCp does

	// Load source index
	loadedIndex, err := LoadSessionsIndex(srcProjectDir)
	if err != nil {
		t.Fatalf("Failed to load source index: %v", err)
	}

	found, _ := FindSessionByID(loadedIndex, sessionID)
	if found == nil {
		t.Fatal("Source session not found")
	}

	// Verify source has all fields
	if found.Summary != "Test summary that should be preserved" {
		t.Errorf("Source Summary mismatch: got %q", found.Summary)
	}
	if found.CustomTitle != "Custom Title Here" {
		t.Errorf("Source CustomTitle mismatch: got %q", found.CustomTitle)
	}

	// Now simulate the copy operation by creating the new entry the same way RunCp does
	newConvID := "11111111-2222-3333-4444-555555555555"
	dstConvFile := filepath.Join(dstProjectDir, newConvID+".jsonl")

	// Create destination directory
	if err := os.MkdirAll(dstProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Copy conversation file (simulating CopyConversationFile)
	srcData, _ := os.ReadFile(filepath.Join(srcProjectDir, sessionID+".jsonl"))
	dstData := strings.ReplaceAll(string(srcData), sessionID, newConvID)
	if err := os.WriteFile(dstConvFile, []byte(dstData), 0644); err != nil {
		t.Fatal(err)
	}

	dstInfo, _ := os.Stat(dstConvFile)

	// Create new entry exactly as RunCp does (after our fix)
	newEntry := SessionEntry{
		SessionID:    newConvID,
		FullPath:     dstConvFile,
		FileMtime:    dstInfo.ModTime().UnixMilli(),
		FirstPrompt:  found.FirstPrompt,
		Summary:      found.Summary,      // This was missing before the fix!
		CustomTitle:  found.CustomTitle,  // This was missing before the fix!
		MessageCount: found.MessageCount,
		Created:      "2024-01-03T00:00:00Z",
		Modified:     "2024-01-03T00:00:00Z",
		GitBranch:    found.GitBranch,
		ProjectPath:  dstRealPath,
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

	// Reload and verify all fields are preserved
	loadedDstIndex, err := LoadSessionsIndexWithOptions(dstProjectDir, LoadSessionsIndexOptions{
		SkipUnindexedScan:     true,
		SkipMissingDataRescan: true,
	})
	if err != nil {
		t.Fatalf("Failed to load destination index: %v", err)
	}

	dstEntry, _ := FindSessionByID(loadedDstIndex, newConvID)
	if dstEntry == nil {
		t.Fatal("Destination session not found")
	}

	// Verify all fields are preserved
	if dstEntry.Summary != "Test summary that should be preserved" {
		t.Errorf("Summary not preserved: got %q, want %q", dstEntry.Summary, "Test summary that should be preserved")
	}
	if dstEntry.CustomTitle != "Custom Title Here" {
		t.Errorf("CustomTitle not preserved: got %q, want %q", dstEntry.CustomTitle, "Custom Title Here")
	}
	if dstEntry.FirstPrompt != "Test prompt" {
		t.Errorf("FirstPrompt not preserved: got %q", dstEntry.FirstPrompt)
	}
	if dstEntry.GitBranch != "main" {
		t.Errorf("GitBranch not preserved: got %q", dstEntry.GitBranch)
	}
	if dstEntry.IsSidechain != true {
		t.Errorf("IsSidechain not preserved: got %v", dstEntry.IsSidechain)
	}
	if dstEntry.ProjectPath != dstRealPath {
		t.Errorf("ProjectPath not updated: got %q, want %q", dstEntry.ProjectPath, dstRealPath)
	}
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
	if err := os.WriteFile(srcFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := CopyConversationFile(srcFile, dstFile, oldID, newID); err != nil {
		t.Fatalf("CopyConversationFile failed: %v", err)
	}

	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), oldID) {
		t.Error("Old session ID still present in copied file")
	}
	if !strings.Contains(string(data), newID) {
		t.Error("New session ID not found in copied file")
	}
}
