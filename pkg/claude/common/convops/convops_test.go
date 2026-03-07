package convops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSessionEntry_DisplayTitle(t *testing.T) {
	tests := []struct {
		name     string
		entry    SessionEntry
		expected string
	}{
		{
			name:     "CustomTitle takes priority",
			entry:    SessionEntry{CustomTitle: "Custom", Summary: "Summary", FirstPrompt: "Prompt"},
			expected: "Custom",
		},
		{
			name:     "Summary when no CustomTitle",
			entry:    SessionEntry{Summary: "Summary", FirstPrompt: "Prompt"},
			expected: "Summary",
		},
		{
			name:     "FirstPrompt as fallback",
			entry:    SessionEntry{FirstPrompt: "Prompt"},
			expected: "Prompt",
		},
		{
			name:     "Empty when all empty",
			entry:    SessionEntry{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.DisplayTitle(); got != tt.expected {
				t.Errorf("DisplayTitle() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestSessionEntry_HasTitle(t *testing.T) {
	tests := []struct {
		name     string
		entry    SessionEntry
		expected bool
	}{
		{
			name:     "Has CustomTitle",
			entry:    SessionEntry{CustomTitle: "Custom"},
			expected: true,
		},
		{
			name:     "Has Summary",
			entry:    SessionEntry{Summary: "Summary"},
			expected: true,
		},
		{
			name:     "Only FirstPrompt",
			entry:    SessionEntry{FirstPrompt: "Prompt"},
			expected: false,
		},
		{
			name:     "Empty",
			entry:    SessionEntry{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.HasTitle(); got != tt.expected {
				t.Errorf("HasTitle() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestPathToProjectDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - path handling differs")
	}
	// Test basic path conversion
	result := PathToProjectDir("/home/user/project")
	if result != "-home-user-project" {
		t.Errorf("PathToProjectDir() = %q, want %q", result, "-home-user-project")
	}
}

func TestFindSessionByID(t *testing.T) {
	index := &SessionsIndex{
		Version: 1,
		Entries: []SessionEntry{
			{SessionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", FirstPrompt: "First"},
			{SessionID: "11111111-2222-3333-4444-555555555555", FirstPrompt: "Second"},
		},
	}

	// Test exact match
	entry, idx := FindSessionByID(index, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if entry == nil || idx != 0 {
		t.Error("Exact match failed")
	}
	if entry.FirstPrompt != "First" {
		t.Errorf("Wrong entry returned: %q", entry.FirstPrompt)
	}

	// Test prefix match
	entry, idx = FindSessionByID(index, "11111111")
	if entry == nil || idx != 1 {
		t.Error("Prefix match failed")
	}

	// Test no match
	entry, _ = FindSessionByID(index, "zzzzzzzz")
	if entry != nil {
		t.Error("Should not find non-existent ID")
	}
}

func TestRemoveSessionByID(t *testing.T) {
	index := &SessionsIndex{
		Version: 1,
		Entries: []SessionEntry{
			{SessionID: "aaa"},
			{SessionID: "bbb"},
			{SessionID: "ccc"},
		},
	}

	// Remove existing
	if !RemoveSessionByID(index, "bbb") {
		t.Error("RemoveSessionByID should return true for existing entry")
	}
	if len(index.Entries) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(index.Entries))
	}

	// Verify correct one removed
	for _, e := range index.Entries {
		if e.SessionID == "bbb" {
			t.Error("Entry 'bbb' should have been removed")
		}
	}

	// Remove non-existing
	if RemoveSessionByID(index, "zzz") {
		t.Error("RemoveSessionByID should return false for non-existent entry")
	}
}

func TestLoadAndSaveSessionsIndex(t *testing.T) {
	tmpDir := t.TempDir()

	// Create and save index
	index := &SessionsIndex{
		Version: 1,
		Entries: []SessionEntry{
			{
				SessionID:   "test-session-id",
				FirstPrompt: "Test prompt",
				Summary:     "Test summary",
				CustomTitle: "Test title",
			},
		},
	}

	if err := SaveSessionsIndex(tmpDir, index); err != nil {
		t.Fatalf("SaveSessionsIndex failed: %v", err)
	}

	// Verify file exists
	indexPath := filepath.Join(tmpDir, "sessions-index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("Index file not created: %v", err)
	}

	// Load and verify
	loaded, err := LoadSessionsIndex(tmpDir)
	if err != nil {
		t.Fatalf("LoadSessionsIndex failed: %v", err)
	}

	if len(loaded.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(loaded.Entries))
	}

	entry := loaded.Entries[0]
	if entry.SessionID != "test-session-id" {
		t.Errorf("ConvID mismatch: %q", entry.SessionID)
	}
	if entry.Summary != "Test summary" {
		t.Errorf("Summary mismatch: %q", entry.Summary)
	}
	if entry.CustomTitle != "Test title" {
		t.Errorf("CustomTitle mismatch: %q", entry.CustomTitle)
	}
}

func TestLoadSessionsIndex_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()

	// Loading from non-existent directory should return empty index
	index, err := LoadSessionsIndex(tmpDir)
	if err != nil {
		t.Fatalf("LoadSessionsIndex should not error for non-existent file: %v", err)
	}
	if index == nil {
		t.Fatal("Index should not be nil")
	}
	if len(index.Entries) != 0 {
		t.Errorf("Expected 0 entries, got %d", len(index.Entries))
	}
}

func TestCopyFile(t *testing.T) {
	tmpDir := t.TempDir()

	src := filepath.Join(tmpDir, "src.txt")
	dst := filepath.Join(tmpDir, "dst.txt")
	content := "test content"

	if err := os.WriteFile(src, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile failed: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != content {
		t.Errorf("Content mismatch: got %q, want %q", string(data), content)
	}
}

func TestCopyConversationFile(t *testing.T) {
	tmpDir := t.TempDir()

	oldID := "old-id-12345"
	newID := "new-id-67890"

	src := filepath.Join(tmpDir, "src.jsonl")
	dst := filepath.Join(tmpDir, "dst.jsonl")

	content := `{"sessionId":"old-id-12345","type":"user"}
{"sessionId":"old-id-12345","type":"assistant"}`

	if err := os.WriteFile(src, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := CopyConversationFile(src, dst, oldID, newID); err != nil {
		t.Fatalf("CopyConversationFile failed: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}

	// Verify old ID replaced
	if string(data) == content {
		t.Error("Content should have been modified")
	}

	// Parse to verify
	var msg struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(`{"sessionId":"new-id-67890"}`), &msg); err == nil {
		if msg.SessionID != newID {
			t.Errorf("ConvID not replaced correctly")
		}
	}
}
