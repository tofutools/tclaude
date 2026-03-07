package conv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseJSONLSession_ExtractsProjectPathAndGitBranch(t *testing.T) {
	// Create a temporary file with JSONL data
	tmpDir := t.TempDir()
	sessionID := "b6075720-0f5f-46e5-b382-efb055c6d25b"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	// Sample JSONL content with cwd and gitBranch
	content := `{"type":"user","cwd":"/home/gigur/git/tofu","sessionId":"b6075720-0f5f-46e5-b382-efb055c6d25b","gitBranch":"main","message":{"role":"user","content":"please say hello"},"timestamp":"2026-02-01T00:35:46.567Z"}
{"type":"assistant","cwd":"/home/gigur/git/tofu","sessionId":"b6075720-0f5f-46e5-b382-efb055c6d25b","gitBranch":"main","message":{"role":"assistant","content":"Hello!"},"timestamp":"2026-02-01T00:35:48.557Z"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Parse the session
	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	// Verify ProjectPath is extracted
	if entry.ProjectPath != "/home/gigur/git/tofu" {
		t.Errorf("Expected ProjectPath '/home/gigur/git/tofu', got '%s'", entry.ProjectPath)
	}

	// Verify GitBranch is extracted
	if entry.GitBranch != "main" {
		t.Errorf("Expected GitBranch 'main', got '%s'", entry.GitBranch)
	}

	// Verify FirstPrompt is extracted
	if entry.FirstPrompt != "please say hello" {
		t.Errorf("Expected FirstPrompt 'please say hello', got '%s'", entry.FirstPrompt)
	}
}

func TestParseJSONLSession_ExtractsFromFirstMessageWithData(t *testing.T) {
	// Test that project path and git branch are extracted from the first message that has them,
	// even if earlier messages are missing those fields
	tmpDir := t.TempDir()
	sessionID := "test-session-id-00000000000000000"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	// First message has no cwd/gitBranch, second does
	content := `{"type":"file-history-snapshot","timestamp":"2026-02-01T00:35:46.000Z"}
{"type":"user","cwd":"/projects/myapp","gitBranch":"feature-branch","message":{"role":"user","content":"test prompt"},"timestamp":"2026-02-01T00:35:46.567Z"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	if entry.ProjectPath != "/projects/myapp" {
		t.Errorf("Expected ProjectPath '/projects/myapp', got '%s'", entry.ProjectPath)
	}

	if entry.GitBranch != "feature-branch" {
		t.Errorf("Expected GitBranch 'feature-branch', got '%s'", entry.GitBranch)
	}
}
