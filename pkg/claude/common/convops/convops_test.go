package convops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal(t, tt.expected, tt.entry.DisplayTitle(), "DisplayTitle()")
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
			assert.Equal(t, tt.expected, tt.entry.HasTitle(), "HasTitle()")
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
		"":             false, // empty
		"worker":       false, // no -x at all
		"unix":         false, // ends in x but no hyphen — not a marker
		"x":            false, // single 'x' isn't `-x`
		"foo-x":        true,  // simplest match
		"worker-r-1-x": true,  // archived reincarnate-1 form
		"worker-c-2-x": true,  // archived clone form (unusual but possible)
		"foo-x-x":      true,  // already-archived-twice (edge case; reincarnate skips this but still detected)
		"foo-extra":    false, // ends in something other than -x
		"-x":           true,  // bare suffix is technically a match — title shouldn't be just "-x" but be permissive
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, IsArchivedTitle(in), "IsArchivedTitle(%q)", in)
			// Method form mirrors the helper.
			e := SessionEntry{CustomTitle: in}
			assert.Equal(t, want, e.IsArchived(), "(SessionEntry).IsArchived() with CustomTitle=%q", in)
		})
	}

	// IsArchived only checks CustomTitle, not Summary / FirstPrompt —
	// a Summary that happens to end in `-x` is coincidence, not an
	// archive mark.
	e := SessionEntry{Summary: "summary-x", FirstPrompt: "prompt-x"}
	assert.False(t, e.IsArchived(), "Summary/FirstPrompt ending in -x must NOT mark a conv as archived")
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
			assert.Equal(t, tc.wantArchd, tc.entry.IsArchived(), "IsArchived() (entry=%+v)", tc.entry)
		})
	}
}

func TestPathToProjectDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows - path handling differs")
	}
	// Test basic path conversion
	result := PathToProjectDir("/home/user/project")
	assert.Equal(t, "-home-user-project", result, "PathToProjectDir()")
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
	require.NotNil(t, entry, "Exact match failed")
	require.Equal(t, 0, idx, "Exact match failed")
	assert.Equal(t, "First", entry.FirstPrompt, "Wrong entry returned")

	// Test prefix match
	entry, idx = FindSessionByID(index, "11111111")
	if assert.NotNil(t, entry, "Prefix match failed") {
		assert.Equal(t, 1, idx, "Prefix match failed")
	}

	// Test no match
	entry, _ = FindSessionByID(index, "zzzzzzzz")
	assert.Nil(t, entry, "Should not find non-existent ID")
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
	assert.True(t, RemoveSessionByID(index, "bbb"), "RemoveSessionByID should return true for existing entry")
	assert.Len(t, index.Entries, 2, "Expected 2 entries")

	// Verify correct one removed
	for _, e := range index.Entries {
		assert.NotEqual(t, "bbb", e.SessionID, "Entry 'bbb' should have been removed")
	}

	// Remove non-existing
	assert.False(t, RemoveSessionByID(index, "zzz"), "RemoveSessionByID should return false for non-existent entry")
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
	require.NoError(t, os.WriteFile(indexPath, []byte(seed), 0600), "seed")

	// Replace aaa with a new payload; bbb is untouched.
	require.NoError(t, UpsertSessionsIndexEntry(tmpDir, SessionEntry{SessionID: "aaa", FirstPrompt: "new"}), "Upsert (replace) failed")

	// Add a brand-new entry ccc.
	require.NoError(t, UpsertSessionsIndexEntry(tmpDir, SessionEntry{SessionID: "ccc", FirstPrompt: "ccc-prompt"}), "Upsert (insert) failed")

	// Remove bbb.
	require.NoError(t, RemoveSessionsIndexEntry(tmpDir, "bbb"), "Remove failed")

	data, err := os.ReadFile(indexPath)
	require.NoError(t, err, "read")

	// Parse loosely to assert unknown fields survived.
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &top), "unparseable JSON")
	assert.Equal(t, `"preserve-me"`, string(top["futureToplevelField"]), "unknown top-level field dropped")
	var entries []map[string]any
	require.NoError(t, json.Unmarshal(top["entries"], &entries), "entries unparseable")
	require.Len(t, entries, 2, "expected 2 entries (aaa updated, ccc inserted, bbb removed); got %+v", entries)
	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["sessionId"].(string)
		byID[id] = e
	}
	if assert.NotNil(t, byID["aaa"], "aaa missing after replace") {
		assert.Equal(t, "new", byID["aaa"]["firstPrompt"], "aaa not replaced: %+v", byID["aaa"])
	}
	assert.NotNil(t, byID["ccc"], "ccc missing after insert")
	_, ok := byID["bbb"]
	assert.False(t, ok, "bbb not removed")
}

// When the file doesn't exist, helpers no-op — they never create it.
func TestSessionsIndex_NoFileNoCreate(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, UpsertSessionsIndexEntry(tmpDir, SessionEntry{SessionID: "aaa"}), "upsert into missing file should no-op")
	require.NoError(t, RemoveSessionsIndexEntry(tmpDir, "aaa"), "remove from missing file should no-op")
	_, err := os.Stat(filepath.Join(tmpDir, "sessions-index.json"))
	assert.True(t, os.IsNotExist(err), "file should not have been created (err=%v)", err)
}

func TestLoadSessionsIndex_NonExistent(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()

	// Loading from non-existent directory should return empty index.
	index, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex should not error for non-existent file")
	require.NotNil(t, index, "Index should not be nil")
	assert.Len(t, index.Entries, 0, "Expected 0 entries")
}

func TestLoadSessionsIndex_ScansJsonlOnDisk(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	content := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"hello"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"my-agent","sessionId":"` + sessionID + `"}
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, sessionID+".jsonl"), []byte(content), 0o600), "write jsonl")

	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex")
	require.Len(t, idx.Entries, 1, "Expected 1 entry")
	got := idx.Entries[0]
	assert.Equal(t, sessionID, got.SessionID, "SessionID")
	assert.Equal(t, "my-agent", got.CustomTitle, "CustomTitle")
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
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, sessionID+".jsonl"), []byte(content), 0o600), "write jsonl")

	// First call: stub gets written to the DB.
	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex (first)")
	require.Len(t, idx.Entries, 0, "first call: expected 0 entries (stub should not surface), got %+v", idx.Entries)

	// Stub should be persisted so we don't re-scan on startup.
	row, err := db.GetConvIndex(sessionID)
	require.NoError(t, err, "stub row should be persisted in DB")
	require.NotNil(t, row, "stub row should be persisted in DB")
	assert.Equal(t, "", row.Created, "stub row Created should be empty")

	// Second call: hits the freshness-passes-cache branch. Stub must
	// still be filtered out.
	idx2, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex (second)")
	require.Len(t, idx2.Entries, 0, "second call: expected 0 entries, got %+v", idx2.Entries)

	// LoadEntriesFromDB (the watch-mode fast path) must also filter.
	dbEntries, err := LoadEntriesFromDB(tmpDir)
	require.NoError(t, err, "LoadEntriesFromDB")
	require.Len(t, dbEntries, 0, "LoadEntriesFromDB: expected 0 entries, got %+v", dbEntries)
}

// Regression: a conversation's git branch can change mid-session — the
// agent runs `git checkout` or moves into a fresh worktree. Claude
// Code stamps the *current* branch onto every .jsonl turn, so
// parseJSONLSession keeps two values: GitBranch is LAST-wins (where
// the agent is now) and GitBranchStartup is FIRST-wins (the branch it
// launched on, immutable). Before the GitBranch fix it was first-wins
// only, which froze `agent ls` and the dashboard on the launch branch.
//
// The project path, by contrast, stays first-wins — cwd is fixed for
// the life of a conversation — so this also pins that the fields
// don't share a capture rule.
func TestParseJSONLSession_GitBranchFirstAndLastWins(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Starts on main, branches to feature-x, then keeps working on
	// feature-x. cwd is identical throughout.
	content := `{"type":"user","cwd":"/myproject","gitBranch":"main","message":{"role":"user","content":"start work"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"user","cwd":"/myproject","gitBranch":"feature-x","message":{"role":"user","content":"after git checkout -b"},"timestamp":"2026-03-01T10:05:00Z"}
{"type":"user","cwd":"/myproject","gitBranch":"feature-x","message":{"role":"user","content":"more work"},"timestamp":"2026-03-01T10:10:00Z"}
`
	path := filepath.Join(tmpDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write jsonl")

	entry := ParseJSONLSessionPublic(path, sessionID)
	require.NotNil(t, entry, "parseJSONLSession returned nil")
	assert.Equal(t, "feature-x", entry.GitBranch, "GitBranch should be the LAST branch seen")
	assert.Equal(t, "main", entry.GitBranchStartup, "GitBranchStartup should be the FIRST branch seen")
	assert.Equal(t, "/myproject", entry.ProjectPath, "ProjectPath stays first-wins")
}

func TestCopyFile(t *testing.T) {
	tmpDir := t.TempDir()

	src := filepath.Join(tmpDir, "src.txt")
	dst := filepath.Join(tmpDir, "dst.txt")
	content := "test content"

	require.NoError(t, os.WriteFile(src, []byte(content), 0644))

	require.NoError(t, CopyFile(src, dst), "CopyFile failed")

	data, err := os.ReadFile(dst)
	require.NoError(t, err)

	assert.Equal(t, content, string(data), "Content mismatch")
}

func TestCopyConversationFile(t *testing.T) {
	tmpDir := t.TempDir()

	oldID := "old-id-12345"
	newID := "new-id-67890"

	src := filepath.Join(tmpDir, "src.jsonl")
	dst := filepath.Join(tmpDir, "dst.jsonl")

	content := `{"sessionId":"old-id-12345","type":"user"}
{"sessionId":"old-id-12345","type":"assistant"}`

	require.NoError(t, os.WriteFile(src, []byte(content), 0644))

	require.NoError(t, CopyConversationFile(src, dst, oldID, newID), "CopyConversationFile failed")

	data, err := os.ReadFile(dst)
	require.NoError(t, err)

	// Verify old ID replaced
	assert.NotEqual(t, content, string(data), "Content should have been modified")

	// Parse to verify
	var msg struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(`{"sessionId":"new-id-67890"}`), &msg); err == nil {
		assert.Equal(t, newID, msg.SessionID, "ConvID not replaced correctly")
	}
}
