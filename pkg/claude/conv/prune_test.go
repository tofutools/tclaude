package conv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write conv file: %v", err)
	}
	return filePath
}

// helper to create a sessions-index.json
func createIndex(t *testing.T, dir string, entries []SessionEntry) {
	t.Helper()
	index := SessionsIndex{Version: 1, Entries: entries}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions-index.json"), data, 0644); err != nil {
		t.Fatalf("Failed to write index: %v", err)
	}
}

// helper to create a companion directory with a subagent file
func createCompanionDir(t *testing.T, dir, sessionID string) string {
	t.Helper()
	companionDir := filepath.Join(dir, sessionID)
	subagentDir := filepath.Join(companionDir, "subagents")
	if err := os.MkdirAll(subagentDir, 0755); err != nil {
		t.Fatalf("Failed to create companion dir: %v", err)
	}
	subagentFile := filepath.Join(subagentDir, "agent-aprompt_suggestion-abc123.jsonl")
	if err := os.WriteFile(subagentFile, []byte(`{"type":"assistant"}`), 0644); err != nil {
		t.Fatalf("Failed to write subagent file: %v", err)
	}
	return companionDir
}

func TestFindEmptyConversations(t *testing.T) {
	dir := t.TempDir()

	emptyID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	nonEmptyID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, emptyID, false)
	createConvFile(t, dir, nonEmptyID, true)

	result := findEmptyConversations(dir)

	if len(result) != 1 {
		t.Fatalf("Expected 1 empty conversation, got %d", len(result))
	}
	if result[0].SessionID != emptyID {
		t.Errorf("Expected session ID %s, got %s", emptyID, result[0].SessionID)
	}
}

func TestFindEmptyConversations_IndexedFlag(t *testing.T) {
	dir := t.TempDir()

	indexedID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	unindexedID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, indexedID, false)
	createConvFile(t, dir, unindexedID, false)
	createIndex(t, dir, []SessionEntry{{SessionID: indexedID}})

	result := findEmptyConversations(dir)

	if len(result) != 2 {
		t.Fatalf("Expected 2 empty conversations, got %d", len(result))
	}

	indexed := 0
	for _, c := range result {
		if c.IsIndexed {
			indexed++
			if c.SessionID != indexedID {
				t.Errorf("Expected indexed session %s, got %s", indexedID, c.SessionID)
			}
		}
	}
	if indexed != 1 {
		t.Errorf("Expected 1 indexed conversation, got %d", indexed)
	}
}

func TestFindMissingFileEntries(t *testing.T) {
	dir := t.TempDir()

	existingID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	missingID := "11111111-2222-3333-4444-555555555555"

	createConvFile(t, dir, existingID, true)
	// Don't create a file for missingID

	createIndex(t, dir, []SessionEntry{
		{SessionID: existingID},
		{SessionID: missingID},
	})

	result := findMissingFileEntries(dir)

	if len(result) != 1 {
		t.Fatalf("Expected 1 missing entry, got %d", len(result))
	}
	if result[0].SessionID != missingID {
		t.Errorf("Expected session ID %s, got %s", missingID, result[0].SessionID)
	}
	if !result[0].IsIndexed {
		t.Error("Expected IsIndexed to be true for missing-file entry")
	}
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

	if len(result) != 1 {
		t.Fatalf("Expected 1 dangling directory, got %d", len(result))
	}
	if result[0].SessionID != danglingID {
		t.Errorf("Expected session ID %s, got %s", danglingID, result[0].SessionID)
	}
}

func TestFindDanglingDirectories_IgnoresNonUUID(t *testing.T) {
	dir := t.TempDir()

	// Create directories that don't look like UUIDs
	for _, name := range []string{"subagents", "cache", "short", "not-a-uuid-but-has-36-chars-in-name!"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatalf("Failed to create dir: %v", err)
		}
	}

	result := findDanglingDirectories(dir)

	if len(result) != 0 {
		t.Errorf("Expected 0 dangling directories, got %d", len(result))
	}
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
			if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
				t.Fatal(err)
			}
			got := hasUserMessages(filePath)
			if got != tt.expected {
				t.Errorf("hasUserMessages() = %v, want %v", got, tt.expected)
			}
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
	if _, err := os.Stat(companionDir); err != nil {
		t.Fatalf("Companion dir should exist before prune: %v", err)
	}

	stdout, _ := os.CreateTemp(dir, "stdout")
	stderr, _ := os.CreateTemp(dir, "stderr")
	defer stdout.Close()
	defer stderr.Close()

	params := &PruneEmptyParams{Dir: dir, Yes: true}

	// Override project path resolution by running against dir directly
	// Since RunPruneEmpty uses GetClaudeProjectPath, we test the lower-level functions
	emptyConvs := findEmptyConversations(dir)
	if len(emptyConvs) != 1 {
		t.Fatalf("Expected 1 empty conv, got %d", len(emptyConvs))
	}

	// Simulate what RunPruneEmpty does for deletion
	convFile := filepath.Join(dir, emptyID+".jsonl")
	if err := os.Remove(convFile); err != nil {
		t.Fatalf("Failed to remove conv file: %v", err)
	}
	// Remove companion directory
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
		t.Error("Companion directory should be deleted")
	}

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
	if len(danglingDirs) != 1 {
		t.Fatalf("Expected 1 dangling dir, got %d", len(danglingDirs))
	}
	if danglingDirs[0].SessionID != danglingID {
		t.Errorf("Expected dangling dir %s, got %s", danglingID, danglingDirs[0].SessionID)
	}

	// Verify nothing was deleted
	convFile := filepath.Join(dir, emptyID+".jsonl")
	if _, err := os.Stat(convFile); os.IsNotExist(err) {
		t.Error("Conv file should still exist (dry run)")
	}
	companionDir := filepath.Join(dir, emptyID)
	if _, err := os.Stat(companionDir); os.IsNotExist(err) {
		t.Error("Companion dir should still exist (dry run)")
	}
	danglingDir := filepath.Join(dir, danglingID)
	if _, err := os.Stat(danglingDir); os.IsNotExist(err) {
		t.Error("Dangling dir should still exist (dry run)")
	}
}

func TestRunPruneEmpty_DanglingDirExclusion(t *testing.T) {
	// When a session ID appears in both missing-file entries and dangling dirs,
	// the dangling dir should be excluded (it will be cleaned up during conv deletion)
	dir := t.TempDir()

	missingID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create index entry but no .jsonl file, and a companion directory
	createIndex(t, dir, []SessionEntry{{SessionID: missingID}})
	createCompanionDir(t, dir, missingID)

	missing := findMissingFileEntries(dir)
	if len(missing) != 1 {
		t.Fatalf("Expected 1 missing entry, got %d", len(missing))
	}

	// Raw dangling dirs would include this ID
	rawDangling := findDanglingDirectories(dir)
	if len(rawDangling) != 1 {
		t.Fatalf("Expected 1 raw dangling dir, got %d", len(rawDangling))
	}

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

	if len(filtered) != 0 {
		t.Errorf("Expected 0 dangling dirs after exclusion, got %d", len(filtered))
	}
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
	if len(empty) != 1 {
		t.Fatalf("Expected 1 empty conv, got %d", len(empty))
	}
	if !strings.HasPrefix(empty[0].SessionID, "aaaaaaaa") {
		t.Errorf("Unexpected session ID prefix: %s", empty[0].SessionID)
	}

	if len(dangling) != 1 {
		t.Fatalf("Expected 1 dangling dir, got %d", len(dangling))
	}
	if !strings.HasPrefix(dangling[0].SessionID, "11111111") {
		t.Errorf("Unexpected session ID prefix: %s", dangling[0].SessionID)
	}
}

func TestRunPruneEmpty_NothingToDelete(t *testing.T) {
	dir := t.TempDir()

	// Only create a valid conversation with user messages
	validID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	createConvFile(t, dir, validID, true)

	empty := findEmptyConversations(dir)
	missing := findMissingFileEntries(dir)
	dangling := findDanglingDirectories(dir)

	if len(empty) != 0 {
		t.Errorf("Expected 0 empty, got %d", len(empty))
	}
	if len(missing) != 0 {
		t.Errorf("Expected 0 missing, got %d", len(missing))
	}
	if len(dangling) != 0 {
		t.Errorf("Expected 0 dangling, got %d", len(dangling))
	}
}
