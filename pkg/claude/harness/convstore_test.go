package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// withTestDB redirects the SQLite store to a throwaway $HOME for the test,
// mirroring the db package's own setupTestDB. The conv_index helpers then
// operate on a fresh DB we can seed.
func withTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()
	return dir
}

// seedConv upserts a conv_index row. FullPath is left empty so Title's
// RefreshConvIndexEntry returns the row without touching disk.
func seedConv(t *testing.T, convID, projectDir, projectPath, harness string, set func(*db.ConvIndexRow)) {
	t.Helper()
	row := &db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  projectDir,
		ProjectPath: projectPath,
		Created:     "2026-01-01T00:00:00Z",
		Harness:     harness,
	}
	if set != nil {
		set(row)
	}
	require.NoError(t, db.UpsertConvIndex(row))
}

func TestClaudeConvStore_Resolve(t *testing.T) {
	withTestDB(t)
	cs := claudeConvStore{}

	cwd := "/home/u/proj"
	projDir := convops.GetClaudeProjectPath(cwd)
	// Two convs sharing the "ab" prefix, one with a distinct id, all in
	// the same project.
	seedConv(t, "abc11111-1111-1111-1111-111111111111", projDir, cwd, "claude", nil)
	seedConv(t, "abc22222-2222-2222-2222-222222222222", projDir, cwd, "claude", nil)
	seedConv(t, "def33333-3333-3333-3333-333333333333", projDir, cwd, "claude", nil)

	// Exact id → unambiguous even though it shares the "abc" prefix.
	ref, err := cs.Resolve("abc11111-1111-1111-1111-111111111111", cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, cwd, ref.ProjectPath, "ConvRef carries the real cwd")
	assert.Equal(t, "claude", ref.Harness)

	// Unique prefix → resolves.
	ref, err = cs.Resolve("def", cwd, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "def33333-3333-3333-3333-333333333333", ref.ConvID)

	// No match → (nil, nil), not an error.
	ref, err = cs.Resolve("zzz", cwd, false)
	require.NoError(t, err)
	assert.Nil(t, ref)

	// Ambiguous prefix → error, NOT collapsed into not-found.
	ref, err = cs.Resolve("abc", cwd, false)
	require.Error(t, err)
	assert.Nil(t, ref)
	assert.Contains(t, err.Error(), "ambiguous")

	// Empty prefix → (nil, nil).
	ref, err = cs.Resolve("", cwd, false)
	require.NoError(t, err)
	assert.Nil(t, ref)
}

func TestClaudeConvStore_Resolve_LocalVsGlobal(t *testing.T) {
	withTestDB(t)
	cs := claudeConvStore{}

	cwdA, cwdB := "/home/u/a", "/home/u/b"
	seedConv(t, "aaaa1111-1111-1111-1111-111111111111", convops.GetClaudeProjectPath(cwdA), cwdA, "claude", nil)
	seedConv(t, "bbbb2222-2222-2222-2222-222222222222", convops.GetClaudeProjectPath(cwdB), cwdB, "claude", nil)

	// Local resolve in cwdA cannot see cwdB's conv.
	ref, err := cs.Resolve("bbbb", cwdA, false)
	require.NoError(t, err)
	assert.Nil(t, ref, "local resolve must not reach another project")

	// Global resolve finds it.
	ref, err = cs.Resolve("bbbb", cwdA, true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, cwdB, ref.ProjectPath)
}

func TestClaudeConvStore_Title(t *testing.T) {
	withTestDB(t)
	cs := claudeConvStore{}

	// customTitle wins.
	seedConv(t, "1111aaaa-1111-1111-1111-111111111111", "/p", "/p", "claude", func(r *db.ConvIndexRow) {
		r.CustomTitle = "My Title"
		r.Summary = "a summary"
		r.FirstPrompt = "first prompt"
	})
	got, err := cs.Title("1111aaaa-1111-1111-1111-111111111111")
	require.NoError(t, err)
	assert.Equal(t, "My Title", got)

	// summary when no customTitle.
	seedConv(t, "2222bbbb-2222-2222-2222-222222222222", "/p", "/p", "claude", func(r *db.ConvIndexRow) {
		r.Summary = "a summary"
		r.FirstPrompt = "first prompt"
	})
	got, err = cs.Title("2222bbbb-2222-2222-2222-222222222222")
	require.NoError(t, err)
	assert.Equal(t, "a summary", got)

	// first prompt as the last resort.
	seedConv(t, "3333cccc-3333-3333-3333-333333333333", "/p", "/p", "claude", func(r *db.ConvIndexRow) {
		r.FirstPrompt = "first prompt"
	})
	got, err = cs.Title("3333cccc-3333-3333-3333-333333333333")
	require.NoError(t, err)
	assert.Equal(t, "first prompt", got)

	// Unknown conv → ("", nil).
	got, err = cs.Title("no-such-conv")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestClaudeConvStore_ListConvs(t *testing.T) {
	withTestDB(t)
	cs := claudeConvStore{}

	// Two convs in different projects, seeded in the cache.
	cwdA, cwdB := "/home/u/a", "/home/u/b"
	seedConv(t, "aaaa1111-1111-1111-1111-111111111111", convops.GetClaudeProjectPath(cwdA), cwdA, "claude", nil)
	seedConv(t, "bbbb2222-2222-2222-2222-222222222222", convops.GetClaudeProjectPath(cwdB), cwdB, "claude", nil)

	// Empty cwd → all conversations across projects (cache path).
	all, err := cs.ListConvs("")
	require.NoError(t, err)
	assert.Len(t, all, 2, "empty cwd lists every indexed conv")

	// A specific cwd → only that project. LoadSessionsIndex scans the
	// project dir on disk, so seed a real .jsonl there (a DB-only row with
	// no file would be evicted by the disk-vs-cache reconciliation).
	projDir := convops.GetClaudeProjectPath(cwdA)
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	convID := "ccccdddd-4444-4444-4444-444444444444"
	line := `{"type":"user","sessionId":"` + convID + `","timestamp":"2026-01-02T00:00:00Z","cwd":"` + cwdA + `","message":{"role":"user","content":"hello from a"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(projDir, convID+".jsonl"), []byte(line), 0o644))

	got, err := cs.ListConvs(cwdA)
	require.NoError(t, err)
	var found bool
	for _, e := range got {
		assert.Equal(t, cwdA, e.ProjectPath, "every listed conv belongs to cwdA")
		if e.SessionID == convID {
			found = true
			assert.Equal(t, "hello from a", e.FirstPrompt)
		}
	}
	assert.True(t, found, "the on-disk conv is listed for its cwd")
}

// TestClaudeHarness_HasConvStore pins the descriptor wiring: the default
// harness exposes a non-nil ConvStore.
func TestClaudeHarness_HasConvStore(t *testing.T) {
	require.NotNil(t, Default().Convs, "claude harness must expose a ConvStore")
}
