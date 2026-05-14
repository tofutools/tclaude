package conv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, os.WriteFile(jsonlPath, []byte(jsonlContent), 0600))

	info, _ := os.Stat(jsonlPath)

	// Pre-populate DB with entry that has matching mtime. Created is
	// set because a real (non-stub) row always has a firstTimestamp
	// from parseJSONLSession — listing surfaces filter rows where
	// Created is empty (see isStubRow).
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    jsonlPath,
		FileMtime:   info.ModTime().Unix(),
		FirstPrompt: "cached prompt",
		Created:     "2026-03-01T10:00:00Z",
		IndexedAt:   info.ModTime(),
	}))

	// Load - should use DB cache, not rescan the file
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err)

	require.Len(t, index.Entries, 1, "expected 1 entry")
	e := index.Entries[0]
	// Should have the cached value, not the file value
	assert.Equal(t, "cached prompt", e.FirstPrompt, "expected cached FirstPrompt")
	assert.Empty(t, e.CustomTitle, "expected empty CustomTitle (cached)")
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
	require.NoError(t, os.WriteFile(jsonlPath, []byte(jsonlContent), 0600))

	// Pre-populate DB with stale entry (old mtime)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    jsonlPath,
		FileMtime:   1, // old mtime - will trigger rescan
		FirstPrompt: "old cached prompt",
	}))

	// Load - should detect stale and rescan
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err)

	require.Len(t, index.Entries, 1, "expected 1 entry")
	e := index.Entries[0]
	assert.Equal(t, "renamed-conv", e.CustomTitle)
	assert.Equal(t, "real user prompt", e.FirstPrompt)
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
	require.NoError(t, os.WriteFile(jsonlPath, []byte(jsonlContent), 0600))

	info, _ := os.Stat(jsonlPath)

	// Pre-populate DB with matching mtime (normally would not rescan)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    jsonlPath,
		FileMtime:   info.ModTime().Unix(),
		FirstPrompt: "old",
	}))

	// ForceRescan should rescan despite matching mtime
	index, err := LoadSessionsIndexWithOptions(dir, LoadSessionsIndexOptions{
		ForceRescan: true,
	})
	require.NoError(t, err)

	e := index.Entries[0]
	assert.Equal(t, "force-found", e.CustomTitle)
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
	require.NoError(t, os.WriteFile(jsonlPath, []byte(jsonlContent), 0600))

	// Load - no DB entry exists, should scan the file
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err)

	require.Len(t, index.Entries, 1, "expected 1 entry")
	e := index.Entries[0]
	assert.Equal(t, "brand new prompt", e.FirstPrompt)
	assert.Equal(t, "A helpful summary", e.Summary)
	assert.Equal(t, "main", e.GitBranch)

	// Verify it was persisted to DB
	row, err := db.GetConvIndex(sessionID)
	require.NoError(t, err, "GetConvIndex")
	require.NotNil(t, row, "expected DB entry after scan")
	assert.Equal(t, "brand new prompt", row.FirstPrompt, "DB FirstPrompt mismatch")
}

func TestDBCache_DeletedFileRemovedFromDB(t *testing.T) {
	setupTestDB(t)
	dir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Pre-populate DB with an entry
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      sessionID,
		ProjectDir:  dir,
		FullPath:    filepath.Join(dir, sessionID+".jsonl"),
		FileMtime:   12345,
		FirstPrompt: "will be removed",
	}))

	// Load - file doesn't exist on disk, should remove from DB
	index, err := LoadSessionsIndex(dir)
	require.NoError(t, err)

	assert.Empty(t, index.Entries, "expected 0 entries")

	// Verify removed from DB
	row, err := db.GetConvIndex(sessionID)
	require.NoError(t, err, "GetConvIndex")
	assert.Nil(t, row, "expected DB entry to be deleted")
}
