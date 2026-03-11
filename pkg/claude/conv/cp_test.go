package conv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRunCp_PreservesAllFields(t *testing.T) {
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

	// Create source conversation file with all metadata inline
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	convContent := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + srcRealPath + `","gitBranch":"main","message":{"role":"user","content":"Test prompt"},"timestamp":"2024-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"Hello"},"timestamp":"2024-01-01T00:00:05Z"}
{"type":"custom-title","customTitle":"Custom Title Here","sessionId":"` + sessionID + `"}
{"type":"summary","summary":"Test summary that should be preserved","timestamp":"2024-01-01T00:05:00Z"}
`
	if err := os.WriteFile(filepath.Join(srcProjectDir, sessionID+".jsonl"), []byte(convContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Load source - scans .jsonl and populates DB
	loadedIndex, err := LoadSessionsIndex(srcProjectDir)
	if err != nil {
		t.Fatalf("Failed to load source index: %v", err)
	}

	found, _ := FindSessionByID(loadedIndex, sessionID)
	if found == nil {
		t.Fatal("Source session not found")
	}

	// Verify source has all fields from scanning
	if found.Summary != "Test summary that should be preserved" {
		t.Errorf("Source Summary mismatch: got %q", found.Summary)
	}
	if found.CustomTitle != "Custom Title Here" {
		t.Errorf("Source CustomTitle mismatch: got %q", found.CustomTitle)
	}
	if found.GitBranch != "main" {
		t.Errorf("Source GitBranch mismatch: got %q", found.GitBranch)
	}

	// Simulate copy: create destination with new UUID
	newConvID := "11111111-2222-3333-4444-555555555555"
	if err := os.MkdirAll(dstProjectDir, 0755); err != nil {
		t.Fatal(err)
	}
	srcData, _ := os.ReadFile(filepath.Join(srcProjectDir, sessionID+".jsonl"))
	dstData := strings.ReplaceAll(string(srcData), sessionID, newConvID)
	dstConvFile := filepath.Join(dstProjectDir, newConvID+".jsonl")
	if err := os.WriteFile(dstConvFile, []byte(dstData), 0644); err != nil {
		t.Fatal(err)
	}

	// Load destination - scans .jsonl and populates DB
	loadedDstIndex, err := LoadSessionsIndex(dstProjectDir)
	if err != nil {
		t.Fatalf("Failed to load destination index: %v", err)
	}

	dstEntry, _ := FindSessionByID(loadedDstIndex, newConvID)
	if dstEntry == nil {
		t.Fatal("Destination session not found")
	}

	// Verify all fields are preserved
	if dstEntry.Summary != "Test summary that should be preserved" {
		t.Errorf("Summary not preserved: got %q", dstEntry.Summary)
	}
	if dstEntry.CustomTitle != "Custom Title Here" {
		t.Errorf("CustomTitle not preserved: got %q", dstEntry.CustomTitle)
	}
	if dstEntry.FirstPrompt != "Test prompt" {
		t.Errorf("FirstPrompt not preserved: got %q", dstEntry.FirstPrompt)
	}
	if dstEntry.GitBranch != "main" {
		t.Errorf("GitBranch not preserved: got %q", dstEntry.GitBranch)
	}

	// Verify DB was populated for destination
	row, err := db.GetConvIndex(newConvID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("expected DB entry for destination conv")
	}
	if row.Summary != "Test summary that should be preserved" {
		t.Errorf("DB Summary mismatch: got %q", row.Summary)
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
