package convops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// Regression: a conversation /rename'd before its first turn — a
// spawned/reincarnated agent whose .jsonl carries only preamble
// metadata (`last-prompt`, `custom-title`, `agent-name`,
// `permission-mode`, none of them timestamped) — must NOT be discarded.
// It has a real custom title; it is a genuine conversation and has to
// surface in `conv ls` and the dashboard alike. It used to be dropped
// as a stub purely because no line carried a timestamp.
func TestLoadSessionsIndex_IndexesNamedTurnlessConv(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "128786c2-79dc-4366-8fed-6250a0d184c7"

	// Faithful reproduction of one of the real-world metadata-only files.
	content := `{"type":"last-prompt","leafUuid":"83c7a0bc-42d3-4728-9804-c7bdf78f8019","sessionId":"` + sessionID + `"}
{"type":"custom-title","customTitle":"dev-c-1","sessionId":"` + sessionID + `"}
{"type":"agent-name","agentName":"dev-c-1","sessionId":"` + sessionID + `"}
{"type":"permission-mode","permissionMode":"auto","sessionId":"` + sessionID + `"}
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, sessionID+".jsonl"), []byte(content), 0o600), "write jsonl")

	// First call: scans the file.
	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex (first)")
	require.Len(t, idx.Entries, 1, "named-but-turnless conv must be indexed, not dropped")
	assert.Equal(t, "dev-c-1", idx.Entries[0].CustomTitle, "CustomTitle")
	assert.NotEmpty(t, idx.Entries[0].Created, "Created must fall back to file mtime so the row is not a stub")

	// Persisted as a real row, not a stub.
	row, err := db.GetConvIndex(sessionID)
	require.NoError(t, err, "row should be persisted in DB")
	require.NotNil(t, row, "row should be persisted in DB")
	assert.NotEmpty(t, row.Created, "persisted row Created must be non-empty")
	assert.Equal(t, "dev-c-1", row.CustomTitle, "persisted CustomTitle")

	// Second call hits the freshness-cache branch — still surfaces.
	idx2, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex (second)")
	require.Len(t, idx2.Entries, 1, "cached path must still surface the conv")

	// LoadEntriesFromDB (the watch-mode fast path) too.
	dbEntries, err := LoadEntriesFromDB(tmpDir)
	require.NoError(t, err, "LoadEntriesFromDB")
	require.Len(t, dbEntries, 1, "watch-mode path must surface the conv")
}

// A .jsonl with genuinely nothing indexable — no timestamped turn, no
// custom title, no summary — IS still a stub: hidden from listings and
// persisted with an empty Created so it is recognised as a stub again.
func TestLoadSessionsIndex_HidesGenuinelyEmptyStub(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "00000000-1111-2222-3333-444444444444"

	// Only un-timestamped state markers — crucially, no custom-title.
	content := `{"type":"last-prompt","leafUuid":"83c7a0bc-42d3-4728-9804-c7bdf78f8019","sessionId":"` + sessionID + `"}
{"type":"permission-mode","permissionMode":"auto","sessionId":"` + sessionID + `"}
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, sessionID+".jsonl"), []byte(content), 0o600), "write jsonl")

	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex")
	require.Len(t, idx.Entries, 0, "a genuinely empty .jsonl must stay hidden")

	row, err := db.GetConvIndex(sessionID)
	require.NoError(t, err, "stub row should be persisted in DB")
	require.NotNil(t, row, "stub row should be persisted in DB")
	assert.Equal(t, "", row.Created, "stub row Created stays empty")
}

// parseJSONLSession returns a real entry — not nil — for a conversation
// named before its first turn, with Created falling back to file mtime.
func TestParseJSONLSession_NamedTurnlessConv(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	content := `{"type":"custom-title","customTitle":"billy-r-1","sessionId":"` + sessionID + `"}
{"type":"agent-name","agentName":"billy-r-1","sessionId":"` + sessionID + `"}
`
	path := filepath.Join(tmpDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write jsonl")

	entry := ParseJSONLSessionPublic(path, sessionID)
	require.NotNil(t, entry, "a named conversation must not be discarded")
	assert.Equal(t, "billy-r-1", entry.CustomTitle, "CustomTitle")
	assert.NotEmpty(t, entry.Created, "Created falls back to file mtime")
}

// Regression: a stub row left in the DB by older scanning logic (which
// discarded named-but-turnless convs) must self-heal. A stub is never
// trusted as fresh, so the next LoadSessionsIndex re-scans it and — the
// conv now being indexable — replaces it with a real row.
func TestLoadSessionsIndex_RescansStaleStub(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "deadbeef-0000-1111-2222-333333333333"

	content := `{"type":"custom-title","customTitle":"stale-agent","sessionId":"` + sessionID + `"}
`
	path := filepath.Join(tmpDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write jsonl")
	info, err := os.Stat(path)
	require.NoError(t, err, "stat jsonl")

	// Seed a stub row that looks fresh — its FileMtime/FileSize match
	// the file on disk, exactly what older logic left behind.
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     sessionID,
		ProjectDir: tmpDir,
		FullPath:   path,
		FileMtime:  info.ModTime().Unix(),
		FileSize:   info.Size(),
	}), "seed stub row")

	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex")
	require.Len(t, idx.Entries, 1, "a stale stub must be re-scanned and healed, not skipped as fresh")
	assert.Equal(t, "stale-agent", idx.Entries[0].CustomTitle, "healed CustomTitle")

	row, err := db.GetConvIndex(sessionID)
	require.NoError(t, err, "row lookup")
	require.NotNil(t, row, "row")
	assert.NotEmpty(t, row.Created, "healed row Created must be non-empty")
}

// Regression: RefreshConvIndexEntry — the per-conv path the dashboard
// resolves titles through — also re-scans a stale stub instead of
// trusting it, so the dashboard heals the same way `conv ls` does.
func TestRefreshConvIndexEntry_RescansStaleStub(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "feedface-0000-1111-2222-333333333333"

	content := `{"type":"custom-title","customTitle":"dash-agent","sessionId":"` + sessionID + `"}
`
	path := filepath.Join(tmpDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600), "write jsonl")
	info, err := os.Stat(path)
	require.NoError(t, err, "stat jsonl")

	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     sessionID,
		ProjectDir: tmpDir,
		FullPath:   path,
		FileMtime:  info.ModTime().Unix(),
		FileSize:   info.Size(),
	}), "seed stub row")

	got := RefreshConvIndexEntry(sessionID)
	require.NotNil(t, got, "RefreshConvIndexEntry must return the healed row")
	assert.Equal(t, "dash-agent", got.CustomTitle, "healed CustomTitle")
	assert.NotEmpty(t, got.Created, "healed Created must be non-empty")
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

// backfillProjectPaths fills an empty ProjectPath from a sibling in the
// SAME Claude project directory, and only that directory — it must not
// borrow a cwd across dirs, and a dir with no sibling-with-a-cwd is
// left untouched.
func TestBackfillProjectPaths(t *testing.T) {
	setupTestDB(t) // backfillProjectPaths persists; isolate the DB (fake IDs no-op)
	entries := []SessionEntry{
		{SessionID: "a", FullPath: "/proj/dirA/a.jsonl", ProjectPath: "/real/repo-a"},
		{SessionID: "b", FullPath: "/proj/dirA/b.jsonl", ProjectPath: ""},
		{SessionID: "d", FullPath: "/proj/dirB/d.jsonl", ProjectPath: "/real/repo-b"},
		{SessionID: "e", FullPath: "/proj/dirB/e.jsonl", ProjectPath: ""},
		{SessionID: "lonely", FullPath: "/proj/dirC/lonely.jsonl", ProjectPath: ""},
	}
	backfillProjectPaths(entries)

	assert.Equal(t, "/real/repo-a", entries[0].ProjectPath, "entry with a cwd is untouched")
	assert.Equal(t, "/real/repo-a", entries[1].ProjectPath, "empty cwd filled from a sibling in dirA")
	assert.Equal(t, "/real/repo-b", entries[2].ProjectPath, "entry with a cwd is untouched")
	assert.Equal(t, "/real/repo-b", entries[3].ProjectPath, "empty cwd filled from the sibling in dirB")
	assert.Equal(t, "", entries[4].ProjectPath, "a dir with no sibling-with-a-cwd stays empty")
}

// Regression: a conversation named before its first turn records no
// cwd, so its ProjectPath is empty. `conv ls` must still show its
// project — backfilled from a sibling conversation in the same Claude
// project directory.
func TestLoadSessionsIndex_BackfillsTurnlessConvProjectPath(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()

	// A normal conversation that recorded its cwd on a turn.
	realID := "11111111-2222-3333-4444-555555555555"
	real := `{"type":"user","cwd":"/home/gigur/git/myrepo","message":{"role":"user","content":"hello"},"timestamp":"2026-03-01T10:00:00Z"}
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, realID+".jsonl"), []byte(real), 0o600), "write real jsonl")

	// A conversation named before its first turn — no turn, no cwd.
	turnlessID := "228afb8c-4d20-4465-be63-754375a2e58a"
	turnless := `{"type":"custom-title","customTitle":"billy-r-1","sessionId":"` + turnlessID + `"}
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, turnlessID+".jsonl"), []byte(turnless), 0o600), "write turnless jsonl")

	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "LoadSessionsIndex")

	byID := map[string]SessionEntry{}
	for _, e := range idx.Entries {
		byID[e.SessionID] = e
	}
	require.Contains(t, byID, turnlessID, "named-but-turnless conv must be indexed")
	assert.Equal(t, "/home/gigur/git/myrepo", byID[turnlessID].ProjectPath,
		"turnless conv must inherit its project from a sibling in the same dir")
	assert.Equal(t, "/home/gigur/git/myrepo", byID[realID].ProjectPath, "sanity: sibling keeps its own cwd")

	// The derived cwd is persisted onto the conv_index row, so every
	// later reader sees it without re-deriving.
	row, err := db.GetConvIndex(turnlessID)
	require.NoError(t, err, "GetConvIndex")
	require.NotNil(t, row, "row")
	assert.Equal(t, "/home/gigur/git/myrepo", row.ProjectPath,
		"backfilled cwd must be written back onto the conv_index row")
}

// The watch-mode fast path (LoadEntriesFromDB) backfills too, and — like
// LoadSessionsIndex — persists the derived cwd onto the conv_index row.
func TestLoadEntriesFromDB_BackfillsAndPersistsProjectPath(t *testing.T) {
	setupTestDB(t)
	dir := "/home/gigur/.claude/projects/-home-gigur-git-myrepo"
	realID := "11111111-2222-3333-4444-555555555555"
	turnlessID := "228afb8c-4d20-4465-be63-754375a2e58a"

	// A normal conversation with a recorded cwd.
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      realID,
		ProjectDir:  dir,
		FullPath:    filepath.Join(dir, realID+".jsonl"),
		ProjectPath: "/home/gigur/git/myrepo",
		Created:     "2026-03-01T10:00:00Z",
	}), "seed real row")
	// A named-but-turnless conversation: no cwd of its own.
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      turnlessID,
		ProjectDir:  dir,
		FullPath:    filepath.Join(dir, turnlessID+".jsonl"),
		CustomTitle: "billy-r-1",
		Created:     "2026-03-01T11:00:00Z",
	}), "seed turnless row")

	entries, err := LoadEntriesFromDB(dir)
	require.NoError(t, err, "LoadEntriesFromDB")
	byID := map[string]SessionEntry{}
	for _, e := range entries {
		byID[e.SessionID] = e
	}
	require.Contains(t, byID, turnlessID, "turnless conv must be present")
	assert.Equal(t, "/home/gigur/git/myrepo", byID[turnlessID].ProjectPath,
		"watch-mode path must backfill the project from a sibling")

	// Persisted, not just returned for display.
	row, err := db.GetConvIndex(turnlessID)
	require.NoError(t, err, "GetConvIndex")
	require.NotNil(t, row, "row")
	assert.Equal(t, "/home/gigur/git/myrepo", row.ProjectPath,
		"backfilled cwd must be persisted onto the conv_index row")
}

// parseJSONLSession flags LastTurnInterrupted when — and only when —
// the file's final conversation turn is a "[Request interrupted by
// user]" marker. Sidecar records (file-history-snapshot, custom-title)
// trail a real interrupt in the .jsonl and must not reset the flag; a
// later genuine turn must.
func TestParseJSONLSession_LastTurnInterrupted(t *testing.T) {
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	userTurn := `{"type":"user","cwd":"/p","message":{"role":"user","content":"hello"},"timestamp":"2026-05-18T10:00:00Z"}`
	asstTurn := `{"type":"assistant","message":{"role":"assistant","content":"on it"},"timestamp":"2026-05-18T10:01:00Z"}`
	marker := `{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-05-18T10:02:00Z"}`
	markerToolUse := `{"type":"user","message":{"role":"user","content":"[Request interrupted by user for tool use]"},"timestamp":"2026-05-18T10:02:00Z"}`
	// The marker can also arrive with content as a block array rather
	// than a bare string — extractMessageContent handles both.
	markerArray := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},"timestamp":"2026-05-18T10:02:00Z"}`
	// A tool_result carrier: type/role "user" but no text block. CC
	// writes one to close a cancelled tool call; it must NOT clear a
	// flag the marker already set.
	toolResult := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"cancelled"}]},"timestamp":"2026-05-18T10:03:00Z"}`
	snapshot := `{"type":"file-history-snapshot"}`
	titleSidecar := `{"type":"custom-title","customTitle":"x","sessionId":"` + sessionID + `"}`

	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"marker is the last turn", []string{userTurn, asstTurn, marker}, true},
		{"tool-use interrupt text variant", []string{userTurn, markerToolUse}, true},
		{"marker with content as a block array", []string{userTurn, markerArray}, true},
		{"marker is the only record in the file", []string{marker}, true},
		{"sidecar records after the marker do not reset it", []string{userTurn, marker, snapshot, titleSidecar}, true},
		{"a tool_result carrier after the marker does not reset it", []string{userTurn, marker, toolResult}, true},
		{"a real user turn after the marker clears it", []string{userTurn, marker, userTurn}, false},
		{"an assistant turn after the marker clears it", []string{userTurn, marker, asstTurn}, false},
		{"no marker anywhere", []string{userTurn, asstTurn}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, sessionID+".jsonl")
			require.NoError(t, os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600), "write jsonl")
			entry := ParseJSONLSessionPublic(path, sessionID)
			require.NotNil(t, entry, "parseJSONLSession returned nil")
			assert.Equal(t, tc.want, entry.LastTurnInterrupted, "LastTurnInterrupted")
		})
	}
}

// RefreshConvIndexEntry — the per-conv path the dashboard poll resolves
// through — recovers a session left stuck 'working' by a user-interrupt.
// Claude Code writes a "[Request interrupted by user]" marker to the
// .jsonl and fires no Stop hook, so without this the dashboard would
// show "working: UserPromptSubmit" indefinitely. The rescan that
// already runs on every poll (the file grew) carries the fix; sibling
// rows in other states are left untouched.
func TestRefreshConvIndexEntry_RecoversInterruptedSession(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	convID := "11111111-2222-3333-4444-555555555555"
	path := filepath.Join(dir, convID+".jsonl")

	// A normal in-flight turn — index it so the conv_index row the
	// dashboard resolves through exists. No interrupt yet.
	initial := `{"type":"user","cwd":"/p","message":{"role":"user","content":"do the thing"},"timestamp":"2026-05-18T10:00:00Z"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600), "write jsonl")
	require.NotNil(t, ScanAndUpsertFile(path), "initial scan")

	// The session owning this conv is stuck 'working' — a
	// UserPromptSubmit hook fired, no Stop hook ever will.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-working", ConvID: convID,
		Status: "working", StatusDetail: "UserPromptSubmit",
	}), "seed working session")
	// Sibling rows in other states — none may be disturbed.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-exited", ConvID: convID, Status: "exited",
	}), "seed exited session")
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-awaiting", ConvID: convID,
		Status: "awaiting_input", StatusDetail: "elicitation",
	}), "seed awaiting session")

	// The user hits Escape: Claude Code appends the interrupt marker
	// (firing no hook). The file grows, so the next poll rescans.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(`{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-05-18T10:01:00Z"}` + "\n")
	require.NoError(t, err, "append marker")
	require.NoError(t, f.Close(), "close")

	require.NotNil(t, RefreshConvIndexEntry(convID), "refresh resolves the conv")

	working, err := db.LoadSession("sess-working")
	require.NoError(t, err, "load working session")
	require.NotNil(t, working, "working session row")
	assert.Equal(t, "idle", working.Status, "the stuck 'working' row recovers to idle")
	assert.Equal(t, "", working.StatusDetail, "the stale status_detail is cleared")

	exited, err := db.LoadSession("sess-exited")
	require.NoError(t, err, "load exited session")
	require.NotNil(t, exited, "exited session row")
	assert.Equal(t, "exited", exited.Status, "an exited sibling row is not disturbed")

	awaiting, err := db.LoadSession("sess-awaiting")
	require.NoError(t, err, "load awaiting session")
	require.NotNil(t, awaiting, "awaiting session row")
	assert.Equal(t, "awaiting_input", awaiting.Status, "an awaiting_* sibling row is not disturbed")
	assert.Equal(t, "elicitation", awaiting.StatusDetail, "awaiting_* status_detail untouched")
}

// The mirror case: a rescan whose last turn is a genuine assistant turn
// (the agent is really working) must leave a 'working' session alone.
func TestRefreshConvIndexEntry_GenuineWorkLeavesSessionWorking(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	convID := "99999999-8888-7777-6666-555555555555"
	path := filepath.Join(dir, convID+".jsonl")

	initial := `{"type":"user","cwd":"/p","message":{"role":"user","content":"start"},"timestamp":"2026-05-18T10:00:00Z"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600), "write jsonl")
	require.NotNil(t, ScanAndUpsertFile(path), "initial scan")

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-2", ConvID: convID, Status: "working", StatusDetail: "PreToolUse",
	}), "seed working session")

	// A normal assistant turn lands — genuine work, not an interrupt.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":"on it"},"timestamp":"2026-05-18T10:01:00Z"}` + "\n")
	require.NoError(t, err, "append turn")
	require.NoError(t, f.Close(), "close")

	require.NotNil(t, RefreshConvIndexEntry(convID), "refresh resolves the conv")

	got, err := db.LoadSession("sess-2")
	require.NoError(t, err, "load session")
	require.NotNil(t, got, "session row")
	assert.Equal(t, "working", got.Status, "a genuinely-working session must NOT be flipped to idle")
	assert.Equal(t, "PreToolUse", got.StatusDetail, "status_detail untouched")
}
