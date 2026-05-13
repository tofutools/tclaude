package convops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setupTestDB isolates the singleton SQLite database under a per-test
// HOME so tests don't share state with each other or the developer's
// real ~/.tclaude/db.sqlite.
func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()
}

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

// TestIsArchivedTitle locks in the suffix detection used by listing
// surfaces (conv ls, dashboard) to default-hide reincarnated old
// convs. Edge case: names like `unix` (no hyphen before x) must NOT
// match — only the literal `-x` suffix counts. Pairs conceptually
// with `groups archive` on the group side; both are soft-delete
// markers.
func TestIsArchivedTitle(t *testing.T) {
	cases := map[string]bool{
		"":                false, // empty
		"worker":          false, // no -x at all
		"unix":            false, // ends in x but no hyphen — not a marker
		"x":               false, // single 'x' isn't `-x`
		"foo-x":           true,  // simplest match
		"worker-r-1-x":    true,  // archived reincarnate-1 form
		"worker-c-2-x":    true,  // archived clone form (unusual but possible)
		"foo-x-x":         true,  // already-archived-twice (edge case; reincarnate skips this but still detected)
		"foo-extra":       false, // ends in something other than -x
		"-x":              true,  // bare suffix is technically a match — title shouldn't be just "-x" but be permissive
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := IsArchivedTitle(in); got != want {
				t.Errorf("IsArchivedTitle(%q) = %v, want %v", in, got, want)
			}
			// Method form mirrors the helper.
			e := SessionEntry{CustomTitle: in}
			if got := e.IsArchived(); got != want {
				t.Errorf("(SessionEntry).IsArchived() with CustomTitle=%q = %v, want %v",
					in, got, want)
			}
		})
	}

	// IsArchived only checks CustomTitle, not Summary / FirstPrompt —
	// a Summary that happens to end in `-x` is coincidence, not an
	// archive mark.
	e := SessionEntry{Summary: "summary-x", FirstPrompt: "prompt-x"}
	if e.IsArchived() {
		t.Error("Summary/FirstPrompt ending in -x must NOT mark a conv as archived")
	}
}

// TestSessionEntry_IsArchived_PrefersColumn covers the canonical
// archived signal: the `ArchivedAt` field (sourced from
// `conv_index.archived_at`). When set, IsArchived returns true even
// if the title doesn't have the `-x` suffix — the column survives
// renames, rescans, etc.
func TestSessionEntry_IsArchived_PrefersColumn(t *testing.T) {
	cases := []struct {
		name      string
		entry     SessionEntry
		wantArchd bool
	}{
		{
			name:      "column set, no -x suffix",
			entry:     SessionEntry{CustomTitle: "worker-r-1", ArchivedAt: "2026-05-10T12:00:00Z"},
			wantArchd: true,
		},
		{
			name:      "column empty, -x suffix legacy",
			entry:     SessionEntry{CustomTitle: "worker-x"},
			wantArchd: true,
		},
		{
			name:      "both set",
			entry:     SessionEntry{CustomTitle: "worker-x", ArchivedAt: "2026-05-10T12:00:00Z"},
			wantArchd: true,
		},
		{
			name:      "neither — active",
			entry:     SessionEntry{CustomTitle: "worker"},
			wantArchd: false,
		},
		{
			name:      "no title, no column",
			entry:     SessionEntry{FirstPrompt: "hello"},
			wantArchd: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.IsArchived(); got != tc.wantArchd {
				t.Errorf("IsArchived() = %v, want %v (entry=%+v)",
					got, tc.wantArchd, tc.entry)
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
		t.Fatal("Exact match failed")
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

// Upsert+Remove perform surgical updates to the legacy
// `sessions-index.json` file. They must preserve unknown top-level
// fields, unknown per-entry fields on other entries, and never create
// the file when it didn't exist (we only maintain it; we don't create it).
func TestSessionsIndex_SurgicalUpdatesPreserveUnknownFields(t *testing.T) {
	tmpDir := t.TempDir()
	indexPath := filepath.Join(tmpDir, "sessions-index.json")

	// Start with a file that has unknown top-level + per-entry fields.
	seed := `{
  "version": 1,
  "futureToplevelField": "preserve-me",
  "entries": [
    {"sessionId": "aaa", "firstPrompt": "old", "futureField": "keep-aaa"},
    {"sessionId": "bbb", "firstPrompt": "bbb-prompt", "futureField": "keep-bbb"}
  ]
}`
	if err := os.WriteFile(indexPath, []byte(seed), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Replace aaa with a new payload; bbb is untouched.
	if err := UpsertSessionsIndexEntry(tmpDir, SessionEntry{SessionID: "aaa", FirstPrompt: "new"}); err != nil {
		t.Fatalf("Upsert (replace) failed: %v", err)
	}

	// Add a brand-new entry ccc.
	if err := UpsertSessionsIndexEntry(tmpDir, SessionEntry{SessionID: "ccc", FirstPrompt: "ccc-prompt"}); err != nil {
		t.Fatalf("Upsert (insert) failed: %v", err)
	}

	// Remove bbb.
	if err := RemoveSessionsIndexEntry(tmpDir, "bbb"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Parse loosely to assert unknown fields survived.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unparseable JSON: %v", err)
	}
	if string(top["futureToplevelField"]) != `"preserve-me"` {
		t.Errorf("unknown top-level field dropped: %s", string(top["futureToplevelField"]))
	}
	var entries []map[string]any
	if err := json.Unmarshal(top["entries"], &entries); err != nil {
		t.Fatalf("entries unparseable: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (aaa updated, ccc inserted, bbb removed); got %d: %+v", len(entries), entries)
	}
	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["sessionId"].(string)
		byID[id] = e
	}
	if byID["aaa"] == nil {
		t.Error("aaa missing after replace")
	} else if byID["aaa"]["firstPrompt"] != "new" {
		t.Errorf("aaa not replaced: %+v", byID["aaa"])
	}
	if byID["ccc"] == nil {
		t.Error("ccc missing after insert")
	}
	if _, ok := byID["bbb"]; ok {
		t.Error("bbb not removed")
	}
}

// When the file doesn't exist, helpers no-op — they never create it.
func TestSessionsIndex_NoFileNoCreate(t *testing.T) {
	tmpDir := t.TempDir()
	if err := UpsertSessionsIndexEntry(tmpDir, SessionEntry{SessionID: "aaa"}); err != nil {
		t.Fatalf("upsert into missing file should no-op: %v", err)
	}
	if err := RemoveSessionsIndexEntry(tmpDir, "aaa"); err != nil {
		t.Fatalf("remove from missing file should no-op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "sessions-index.json")); !os.IsNotExist(err) {
		t.Errorf("file should not have been created (err=%v)", err)
	}
}

func TestLoadSessionsIndex_NonExistent(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()

	// Loading from non-existent directory should return empty index.
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

func TestLoadSessionsIndex_ScansJsonlOnDisk(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	content := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"hello"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"my-agent","sessionId":"` + sessionID + `"}
`
	if err := os.WriteFile(filepath.Join(tmpDir, sessionID+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	idx, err := LoadSessionsIndex(tmpDir)
	if err != nil {
		t.Fatalf("LoadSessionsIndex: %v", err)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(idx.Entries))
	}
	got := idx.Entries[0]
	if got.SessionID != sessionID {
		t.Errorf("SessionID: got %q want %q", got.SessionID, sessionID)
	}
	if got.CustomTitle != "my-agent" {
		t.Errorf("CustomTitle: got %q want %q", got.CustomTitle, "my-agent")
	}
}

// Regression: agent-spawn artifacts — .jsonl files that only carry
// preamble metadata (`last-prompt`, `custom-title`, `agent-name`,
// `permission-mode`) with no timestamps — used to surface as ghost
// rows in `conv ls` after the first scan stored them as stubs in
// conv_index. Stubs must stay in the DB (so we don't pointlessly
// re-scan them on every startup) but never reach listing surfaces.
func TestLoadSessionsIndex_HidesStubFromAgentSpawnArtifact(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "128786c2-79dc-4366-8fed-6250a0d184c7"

	// Faithful reproduction of one of the real-world stub files.
	content := `{"type":"last-prompt","leafUuid":"83c7a0bc-42d3-4728-9804-c7bdf78f8019","sessionId":"` + sessionID + `"}
{"type":"custom-title","customTitle":"dev-c-1","sessionId":"` + sessionID + `"}
{"type":"agent-name","agentName":"dev-c-1","sessionId":"` + sessionID + `"}
{"type":"permission-mode","permissionMode":"auto","sessionId":"` + sessionID + `"}
`
	if err := os.WriteFile(filepath.Join(tmpDir, sessionID+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	// First call: stub gets written to the DB.
	idx, err := LoadSessionsIndex(tmpDir)
	if err != nil {
		t.Fatalf("LoadSessionsIndex (first): %v", err)
	}
	if len(idx.Entries) != 0 {
		t.Fatalf("first call: expected 0 entries (stub should not surface), got %d: %+v", len(idx.Entries), idx.Entries)
	}

	// Stub should be persisted so we don't re-scan on startup.
	if row, err := db.GetConvIndex(sessionID); err != nil || row == nil {
		t.Fatalf("stub row should be persisted in DB; row=%v err=%v", row, err)
	} else if row.Created != "" {
		t.Errorf("stub row Created should be empty, got %q", row.Created)
	}

	// Second call: hits the freshness-passes-cache branch. Stub must
	// still be filtered out.
	idx2, err := LoadSessionsIndex(tmpDir)
	if err != nil {
		t.Fatalf("LoadSessionsIndex (second): %v", err)
	}
	if len(idx2.Entries) != 0 {
		t.Fatalf("second call: expected 0 entries, got %d: %+v", len(idx2.Entries), idx2.Entries)
	}

	// LoadEntriesFromDB (the watch-mode fast path) must also filter.
	dbEntries, err := LoadEntriesFromDB(tmpDir)
	if err != nil {
		t.Fatalf("LoadEntriesFromDB: %v", err)
	}
	if len(dbEntries) != 0 {
		t.Fatalf("LoadEntriesFromDB: expected 0 entries, got %d: %+v", len(dbEntries), dbEntries)
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
