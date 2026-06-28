package convops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// jsonlTitled builds a minimal non-stub .jsonl carrying a custom title — enough
// for parseJSONLSession to produce a real (non-stub) SessionEntry.
func jsonlTitled(title string) []byte {
	return []byte(`{"type":"user","cwd":"/proj","gitBranch":"main","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"` + title + `"}
`)
}

func findEntry(t *testing.T, idx *SessionsIndex, convID string) SessionEntry {
	t.Helper()
	for _, e := range idx.Entries {
		if e.SessionID == convID {
			return e
		}
	}
	t.Fatalf("entry %s not found in index (%d entries)", convID, len(idx.Entries))
	return SessionEntry{}
}

// TestLoadSessionsIndex_ArchivedAtCarriesAcrossRescan covers the load-path
// carry-over (JOH-320 "flicker guard"): archived_at lives only in conv_index,
// so a forced rescan — which rebuilds the entry from the .jsonl — must re-apply
// the DB's archived flag, or a just-archived conv would list as active for one
// pass. The title here is plain (no `-x`), so this proves the COLUMN carries,
// not the title.
func TestLoadSessionsIndex_ArchivedAtCarriesAcrossRescan(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	sessionID := "11111111-2222-3333-4444-555555555555"
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, sessionID+".jsonl"), jsonlTitled("worker"), 0o600))

	_, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err)
	require.NoError(t, db.SetConvIndexArchived(sessionID, true), "stamp archived_at")

	idx, err := LoadSessionsIndexWithOptions(tmpDir, LoadSessionsIndexOptions{ForceRescan: true})
	require.NoError(t, err)

	e := findEntry(t, idx, sessionID)
	assert.True(t, e.IsArchived(),
		"archived_at must survive a forced rescan; got ArchivedAt=%q", e.ArchivedAt)
}

// TestLoadSessionsIndex_DegradedFallbackOnDBReadError covers the fail-closed
// degraded path: when db.ListConvIndex errors (here forced by dropping the
// table, the same deterministic trick the branch-history test uses), the
// authoritative archived_at is unreadable, so the load path falls back to the
// cosmetic `-x` title for that pass — keeping a retired generation hidden
// rather than failing open. A non-`-x` conv stays visible even then.
func TestLoadSessionsIndex_DegradedFallbackOnDBReadError(t *testing.T) {
	setupTestDB(t)
	tmpDir := t.TempDir()
	archivedID := "22222222-3333-4444-5555-666666666666"
	liveID := "33333333-4444-5555-6666-777777777777"
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, archivedID+".jsonl"), jsonlTitled("worker-x"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, liveID+".jsonl"), jsonlTitled("worker"), 0o600))

	// Drop conv_index so db.ListConvIndex errors → dbReadFailed → degraded
	// title fallback. The scan itself must not abort.
	conn, err := db.Open()
	require.NoError(t, err)
	_, err = conn.Exec(`DROP TABLE conv_index`)
	require.NoError(t, err, "drop conv_index to force the db read to fail")

	idx, err := LoadSessionsIndex(tmpDir)
	require.NoError(t, err, "scan must not abort on a db read error")

	arch := findEntry(t, idx, archivedID)
	assert.True(t, arch.IsArchived(),
		"a `-x` title is fail-closed (hidden) while the column is unreadable")
	live := findEntry(t, idx, liveID)
	assert.False(t, live.IsArchived(),
		"a non-`-x` conv stays visible even in degraded mode")
}
