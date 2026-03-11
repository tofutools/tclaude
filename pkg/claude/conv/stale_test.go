package conv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()
}

func TestDBCache_FreshEntryNotRescanned(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Write a .jsonl file
	jsonlContent := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"prompt"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"should-not-appear","sessionId":"` + sessionID + `"}
`
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0600); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(jsonlPath)

	// Pre-populate DB with entry that has matching mtime
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    jsonlPath,
		FileMtime:   info.ModTime().Unix(),
		FirstPrompt: "cached prompt",
		IndexedAt:   info.ModTime(),
	}); err != nil {
		t.Fatal(err)
	}

	// Load - should use DB cache, not rescan the file
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(index.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(index.Entries))
	}
	e := index.Entries[0]
	// Should have the cached value, not the file value
	if e.FirstPrompt != "cached prompt" {
		t.Errorf("expected cached FirstPrompt 'cached prompt', got %q", e.FirstPrompt)
	}
	if e.CustomTitle != "" {
		t.Errorf("expected empty CustomTitle (cached), got %q", e.CustomTitle)
	}
}

func TestDBCache_StaleEntryRescanned(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Write a .jsonl file with a custom title
	jsonlContent := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"real user prompt"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"custom-title","customTitle":"renamed-conv","sessionId":"` + sessionID + `"}
`
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Pre-populate DB with stale entry (old mtime)
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    jsonlPath,
		FileMtime:   1, // old mtime - will trigger rescan
		FirstPrompt: "old cached prompt",
	}); err != nil {
		t.Fatal(err)
	}

	// Load - should detect stale and rescan
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(index.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(index.Entries))
	}
	e := index.Entries[0]
	if e.CustomTitle != "renamed-conv" {
		t.Errorf("expected CustomTitle 'renamed-conv', got %q", e.CustomTitle)
	}
	if e.FirstPrompt != "real user prompt" {
		t.Errorf("expected FirstPrompt 'real user prompt', got %q", e.FirstPrompt)
	}
}

func TestDBCache_ForceRescanIgnoresMtime(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Write a .jsonl file with a custom title
	jsonlContent := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"prompt"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"force-found","sessionId":"` + sessionID + `"}
`
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0600); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(jsonlPath)

	// Pre-populate DB with matching mtime (normally would not rescan)
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    jsonlPath,
		FileMtime:   info.ModTime().Unix(),
		FirstPrompt: "old",
	}); err != nil {
		t.Fatal(err)
	}

	// ForceRescan should rescan despite matching mtime
	index, err := LoadSessionsIndexWithOptions(dir, LoadSessionsIndexOptions{
		ForceRescan: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	e := index.Entries[0]
	if e.CustomTitle != "force-found" {
		t.Errorf("expected CustomTitle 'force-found', got %q", e.CustomTitle)
	}
}

func TestDBCache_NewFileGetsScanned(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Write a .jsonl file with no DB entry (new file)
	jsonlContent := `{"type":"user","cwd":"/myproject","gitBranch":"main","message":{"role":"user","content":"brand new prompt"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"summary","summary":"A helpful summary","timestamp":"2026-03-01T10:05:00Z"}
`
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Load - no DB entry exists, should scan the file
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(index.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(index.Entries))
	}
	e := index.Entries[0]
	if e.FirstPrompt != "brand new prompt" {
		t.Errorf("expected FirstPrompt 'brand new prompt', got %q", e.FirstPrompt)
	}
	if e.Summary != "A helpful summary" {
		t.Errorf("expected Summary 'A helpful summary', got %q", e.Summary)
	}
	if e.GitBranch != "main" {
		t.Errorf("expected GitBranch 'main', got %q", e.GitBranch)
	}

	// Verify it was persisted to DB
	row, err := db.GetConvIndex(sessionID)
	if err != nil {
		t.Fatalf("GetConvIndex: %v", err)
	}
	if row == nil {
		t.Fatal("expected DB entry after scan")
	}
	if row.FirstPrompt != "brand new prompt" {
		t.Errorf("DB FirstPrompt mismatch: got %q", row.FirstPrompt)
	}
}

func TestDBCache_DeletedFileRemovedFromDB(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Pre-populate DB with an entry
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    filepath.Join(dir, sessionID+".jsonl"),
		FileMtime:   12345,
		FirstPrompt: "will be removed",
	}); err != nil {
		t.Fatal(err)
	}

	// Load - file doesn't exist on disk, should remove from DB
	index, err := LoadSessionsIndex(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(index.Entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(index.Entries))
	}

	// Verify removed from DB
	row, err := db.GetConvIndex(sessionID)
	if err != nil {
		t.Fatalf("GetConvIndex: %v", err)
	}
	if row != nil {
		t.Error("expected DB entry to be deleted")
	}
}
