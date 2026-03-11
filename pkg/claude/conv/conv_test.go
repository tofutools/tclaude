package conv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseJSONLSession_ExtractsProjectPathAndGitBranch(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "b6075720-0f5f-46e5-b382-efb055c6d25b"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"user","cwd":"/home/alice/git/myproject","sessionId":"b6075720-0f5f-46e5-b382-efb055c6d25b","gitBranch":"main","message":{"role":"user","content":"please say hello"},"timestamp":"2026-02-01T00:35:46.567Z"}
{"type":"assistant","cwd":"/home/alice/git/myproject","sessionId":"b6075720-0f5f-46e5-b382-efb055c6d25b","gitBranch":"main","message":{"role":"assistant","content":"Hello!"},"timestamp":"2026-02-01T00:35:48.557Z"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	if entry.ProjectPath != "/home/alice/git/myproject" {
		t.Errorf("Expected ProjectPath '/home/alice/git/myproject', got '%s'", entry.ProjectPath)
	}
	if entry.GitBranch != "main" {
		t.Errorf("Expected GitBranch 'main', got '%s'", entry.GitBranch)
	}
	if entry.FirstPrompt != "please say hello" {
		t.Errorf("Expected FirstPrompt 'please say hello', got '%s'", entry.FirstPrompt)
	}
}

func TestParseJSONLSession_ExtractsFromFirstMessageWithData(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-id-00000000000000000"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

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

func TestParseJSONLSession_FindsCustomTitle(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"hello"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"Hi!"},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"progress","timestamp":"2026-03-01T10:00:06Z"}
{"type":"system","timestamp":"2026-03-01T10:00:07Z"}
{"type":"custom-title","customTitle":"initial-greeting","sessionId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	if entry.FirstPrompt != "hello" {
		t.Errorf("Expected FirstPrompt 'hello', got %q", entry.FirstPrompt)
	}
	if entry.CustomTitle != "initial-greeting" {
		t.Errorf("Expected CustomTitle 'initial-greeting', got %q", entry.CustomTitle)
	}
}

func TestParseJSONLSession_FindsSummaryAfterPrompt(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"user","cwd":"/myproject","gitBranch":"main","message":{"role":"user","content":"fix the bug"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"Sure, let me look."},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"summary","summary":"Fixed a critical authentication bug","timestamp":"2026-03-01T10:05:00Z"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	if entry.FirstPrompt != "fix the bug" {
		t.Errorf("Expected FirstPrompt 'fix the bug', got %q", entry.FirstPrompt)
	}
	if entry.Summary != "Fixed a critical authentication bug" {
		t.Errorf("Expected Summary 'Fixed a critical authentication bug', got %q", entry.Summary)
	}
	if entry.ProjectPath != "/myproject" {
		t.Errorf("Expected ProjectPath '/myproject', got %q", entry.ProjectPath)
	}
	if entry.GitBranch != "main" {
		t.Errorf("Expected GitBranch 'main', got %q", entry.GitBranch)
	}
}

func TestParseJSONLSession_StopsWhenAllFound(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"summary","summary":"Resumed session summary","timestamp":"2026-03-01T09:59:00Z"}
{"type":"user","cwd":"/myproject","message":{"role":"user","content":"continue where we left off"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"OK"},"timestamp":"2026-03-01T10:00:05Z"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	if entry.Summary != "Resumed session summary" {
		t.Errorf("Expected Summary 'Resumed session summary', got %q", entry.Summary)
	}
	if entry.FirstPrompt != "continue where we left off" {
		t.Errorf("Expected FirstPrompt 'continue where we left off', got %q", entry.FirstPrompt)
	}
}

func TestParseJSONLSession_FindsSummaryAfterManyLines(t *testing.T) {
	// Summary at end of file after many assistant messages should be found
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	lines := `{"type":"user","cwd":"/proj","message":{"role":"user","content":"refactor the auth module"},"timestamp":"2026-03-01T10:00:00Z"}
`
	for i := 0; i < 100; i++ {
		lines += `{"type":"assistant","message":{"role":"assistant","content":"working on it..."},"timestamp":"2026-03-01T10:00:05Z"}
`
	}
	lines += `{"type":"summary","summary":"Refactored auth module for better separation of concerns","timestamp":"2026-03-01T11:00:00Z"}
`
	if err := os.WriteFile(filePath, []byte(lines), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}

	if entry.FirstPrompt != "refactor the auth module" {
		t.Errorf("Expected FirstPrompt 'refactor the auth module', got %q", entry.FirstPrompt)
	}
	if entry.Summary != "Refactored auth module for better separation of concerns" {
		t.Errorf("Expected Summary 'Refactored auth module for better separation of concerns', got %q", entry.Summary)
	}
}
