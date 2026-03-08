package conv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper to write a sessions-index.json with given entries
func writeTestIndex(t *testing.T, dir string, entries []map[string]any) {
	t.Helper()
	index := map[string]any{
		"version": 1,
		"entries": entries,
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions-index.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// helper to read the raw index back as map[string]any
func readRawIndex(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "sessions-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPatchIndexEntries_UpdatesFields(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "sessions-index.json")

	writeTestIndex(t, dir, []map[string]any{
		{
			"sessionId":  "aaaa-bbbb",
			"fileMtime":  1000,
			"firstPrompt": "old prompt",
		},
		{
			"sessionId":  "cccc-dddd",
			"fileMtime":  2000,
			"firstPrompt": "unchanged",
		},
	})

	patchIndexEntries(indexPath, map[string]map[string]any{
		"aaaa-bbbb": {
			"fileMtime":   5000,
			"firstPrompt": "new prompt",
			"customTitle":  "my-title",
		},
	})

	raw := readRawIndex(t, dir)
	entries := raw["entries"].([]any)

	// First entry should be updated
	e0 := entries[0].(map[string]any)
	if e0["fileMtime"].(float64) != 5000 {
		t.Errorf("expected fileMtime 5000, got %v", e0["fileMtime"])
	}
	if e0["firstPrompt"].(string) != "new prompt" {
		t.Errorf("expected firstPrompt 'new prompt', got %v", e0["firstPrompt"])
	}
	if e0["customTitle"].(string) != "my-title" {
		t.Errorf("expected customTitle 'my-title', got %v", e0["customTitle"])
	}

	// Second entry should be unchanged
	e1 := entries[1].(map[string]any)
	if e1["fileMtime"].(float64) != 2000 {
		t.Errorf("expected fileMtime 2000, got %v", e1["fileMtime"])
	}
	if e1["firstPrompt"].(string) != "unchanged" {
		t.Errorf("expected firstPrompt 'unchanged', got %v", e1["firstPrompt"])
	}
}

func TestPatchIndexEntries_PreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "sessions-index.json")

	writeTestIndex(t, dir, []map[string]any{
		{
			"sessionId":     "aaaa-bbbb",
			"fileMtime":     1000,
			"futureField":   "should survive",
			"anotherField":  42,
		},
	})

	patchIndexEntries(indexPath, map[string]map[string]any{
		"aaaa-bbbb": {"fileMtime": 2000},
	})

	raw := readRawIndex(t, dir)
	e := raw["entries"].([]any)[0].(map[string]any)

	if e["futureField"].(string) != "should survive" {
		t.Errorf("expected futureField preserved, got %v", e["futureField"])
	}
	if e["anotherField"].(float64) != 42 {
		t.Errorf("expected anotherField preserved, got %v", e["anotherField"])
	}
	if e["fileMtime"].(float64) != 2000 {
		t.Errorf("expected fileMtime updated to 2000, got %v", e["fileMtime"])
	}
}

func TestPatchIndexEntries_NoMatchNoWrite(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "sessions-index.json")

	writeTestIndex(t, dir, []map[string]any{
		{"sessionId": "aaaa-bbbb", "fileMtime": 1000},
	})

	before, _ := os.ReadFile(indexPath)

	patchIndexEntries(indexPath, map[string]map[string]any{
		"nonexistent": {"fileMtime": 9999},
	})

	after, _ := os.ReadFile(indexPath)
	if string(before) != string(after) {
		t.Error("expected index file unchanged when no sessions match")
	}
}

func TestStaleRescan_UpdatesIndexOnMtimeDiff(t *testing.T) {
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Write a .jsonl file with a custom title and real prompt
	jsonlContent := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"real user prompt"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"custom-title","customTitle":"renamed-conv","sessionId":"` + sessionID + `"}
`
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Write an index with an old mtime (stale) and outdated firstPrompt
	oldMtime := time.Now().Add(-1 * time.Hour).Unix()
	writeTestIndex(t, dir, []map[string]any{
		{
			"sessionId":   sessionID,
			"fileMtime":   oldMtime,
			"firstPrompt": "<local-command-caveat>system junk</local-command-caveat>",
			"created":     "2026-03-01T10:00:00Z",
			"modified":    "2026-03-01T10:00:00Z",
		},
	})

	// Load with stale rescan enabled
	index, err := LoadSessionsIndexWithOptions(dir, LoadSessionsIndexOptions{
		SkipUnindexedScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify in-memory entry was updated
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

	// Verify the index file was patched on disk
	raw := readRawIndex(t, dir)
	diskEntry := raw["entries"].([]any)[0].(map[string]any)
	if diskEntry["customTitle"] != "renamed-conv" {
		t.Errorf("expected customTitle persisted to disk, got %v", diskEntry["customTitle"])
	}
	if diskEntry["firstPrompt"] != "real user prompt" {
		t.Errorf("expected firstPrompt persisted to disk, got %v", diskEntry["firstPrompt"])
	}
	// fileMtime should be updated to the actual file mtime
	diskMtime := int64(diskEntry["fileMtime"].(float64))
	if diskMtime <= oldMtime {
		t.Errorf("expected fileMtime updated beyond %d, got %d", oldMtime, diskMtime)
	}
}

func TestStaleRescan_SkipsWhenMtimeMatches(t *testing.T) {
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

	// Set the index mtime to match the file's actual mtime
	info, _ := os.Stat(jsonlPath)
	writeTestIndex(t, dir, []map[string]any{
		{
			"sessionId":   sessionID,
			"fileMtime":   info.ModTime().Unix(),
			"firstPrompt": "original prompt",
			"created":     "2026-03-01T10:00:00Z",
			"modified":    "2026-03-01T10:00:00Z",
		},
	})

	index, err := LoadSessionsIndexWithOptions(dir, LoadSessionsIndexOptions{
		SkipUnindexedScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should NOT have rescanned — original data preserved
	e := index.Entries[0]
	if e.FirstPrompt != "original prompt" {
		t.Errorf("expected FirstPrompt unchanged 'original prompt', got %q", e.FirstPrompt)
	}
	if e.CustomTitle != "" {
		t.Errorf("expected CustomTitle empty (no rescan), got %q", e.CustomTitle)
	}
}

func TestStaleRescan_ForceRescanIgnoresMtime(t *testing.T) {
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

	// Set the index mtime to match — normally would skip rescan
	info, _ := os.Stat(jsonlPath)
	writeTestIndex(t, dir, []map[string]any{
		{
			"sessionId":   sessionID,
			"fileMtime":   info.ModTime().Unix(),
			"firstPrompt": "old",
			"created":     "2026-03-01T10:00:00Z",
			"modified":    "2026-03-01T10:00:00Z",
		},
	})

	// ForceRescan should rescan despite matching mtime
	index, err := LoadSessionsIndexWithOptions(dir, LoadSessionsIndexOptions{
		SkipUnindexedScan: true,
		ForceRescan:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	e := index.Entries[0]
	if e.CustomTitle != "force-found" {
		t.Errorf("expected CustomTitle 'force-found', got %q", e.CustomTitle)
	}
}

func TestStaleRescan_SkipsZeroMtime(t *testing.T) {
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Write a .jsonl file with a custom title
	jsonlContent := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"prompt"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"should-not-appear","sessionId":"` + sessionID + `"}
`
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Index entry with fileMtime=0 — should not trigger stale rescan
	writeTestIndex(t, dir, []map[string]any{
		{
			"sessionId":   sessionID,
			"fileMtime":   0,
			"firstPrompt": "original",
			"created":     "2026-03-01T10:00:00Z",
			"modified":    "2026-03-01T10:00:00Z",
		},
	})

	index, err := LoadSessionsIndexWithOptions(dir, LoadSessionsIndexOptions{
		SkipUnindexedScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	e := index.Entries[0]
	if e.CustomTitle != "" {
		t.Errorf("expected no rescan for mtime=0, but CustomTitle was set to %q", e.CustomTitle)
	}
}
