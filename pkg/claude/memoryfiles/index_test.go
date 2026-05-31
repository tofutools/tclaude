package memoryfiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMemDir creates a memory dir with the named (empty) files and an
// optional MEMORY.md body, returning the dir.
func writeMemDir(t *testing.T, index string, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644))
	}
	if index != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, memoryIndexFile), []byte(index), 0o644))
	}
	return dir
}

func TestTargetIsGone(t *testing.T) {
	dir := writeMemDir(t, "", "present.md")

	cases := []struct {
		name         string
		target       string
		alsoMissing  map[string]bool
		treatMissing bool
		wantGone     bool
	}{
		{"present file kept", "present.md", nil, true, false},
		{"missing file gone when treatMissing", "missing.md", nil, true, true},
		{"missing file kept when not treatMissing", "missing.md", nil, false, false},
		{"http url never gone", "https://example.com/x.md", nil, true, false},
		{"scheme url never gone", "file://x.md", nil, true, false},
		{"mailto never gone", "mailto:a@b.c", nil, true, false},
		{"anchor stripped, present file kept", "present.md#section", nil, true, false},
		{"title stripped, present file kept", `present.md "Some Title"`, nil, true, false},
		{"empty target kept", "", nil, true, false},
		{"in alsoMissing is gone even without treatMissing", "present.md", map[string]bool{"present.md": true}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantGone, targetIsGone(dir, tc.target, tc.alsoMissing, tc.treatMissing))
		})
	}
}

func TestTargetIsGone_SpacedAndWrappedDestinations(t *testing.T) {
	dir := writeMemDir(t, "", "present.md", "my notes.md")

	// A real title is stripped and the file resolves → kept.
	assert.False(t, targetIsGone(dir, `present.md "Some Title"`, nil, true))
	// A spaced filename with NO title must not be truncated at the space.
	assert.False(t, targetIsGone(dir, "my notes.md", nil, true))
	// Angle-bracket destination is unwrapped, then resolves → kept.
	assert.False(t, targetIsGone(dir, "<my notes.md>", nil, true))
	// A genuinely missing, title-bearing entry is still gone.
	assert.True(t, targetIsGone(dir, `gone.md "Title"`, nil, true))
}

func TestTargetIsGone_CleanPathNoBasenameCollision(t *testing.T) {
	dir := writeMemDir(t, "") // clean path: existence on disk irrelevant
	del := map[string]bool{"notes.md": true}

	// The deleted top-level file matches (and its ./ form).
	assert.True(t, targetIsGone(dir, "notes.md", del, false))
	assert.True(t, targetIsGone(dir, "./notes.md", del, false))
	// A subpath entry sharing only the basename must NOT match.
	assert.False(t, targetIsGone(dir, "archive/notes.md", del, false))
}

func TestPruneIndexContent_CleanKeepsSubpathSharingBasename(t *testing.T) {
	dir := writeMemDir(t, "")
	content := "- [Top](notes.md) — deleted\n- [Sub](archive/notes.md) — different file\n"

	out, removed := pruneIndexContent(content, dir, map[string]bool{"notes.md": true}, false)
	require.Len(t, removed, 1)
	assert.Equal(t, "notes.md", removed[0].target)
	assert.Contains(t, out, "archive/notes.md") // subpath entry survives
}

func TestPruneIndexContent_RemovesOnlyDanglingListItems(t *testing.T) {
	dir := writeMemDir(t, "", "keep_me.md")

	content := "# Memory Index\n" +
		"\n" +
		"> 📌 some blockquote with a [link](https://example.com)\n" +
		"\n" +
		"- [Keep](keep_me.md) — still on disk\n" +
		"- [Gone](project_pappfigur.md) — deleted file\n" +
		"- [Docs](https://example.com/docs) — a URL, never pruned\n" +
		"- a plain bullet with no link\n"

	out, removed := pruneIndexContent(content, dir, nil, true)

	require.Len(t, removed, 1)
	assert.Equal(t, "project_pappfigur.md", removed[0].target)
	assert.Contains(t, removed[0].line, "[Gone](project_pappfigur.md)")

	// The dangling line is gone; everything else stays verbatim.
	assert.NotContains(t, out, "project_pappfigur.md")
	assert.Contains(t, out, "# Memory Index")
	assert.Contains(t, out, "> 📌 some blockquote")
	assert.Contains(t, out, "- [Keep](keep_me.md)")
	assert.Contains(t, out, "- [Docs](https://example.com/docs)")
	assert.Contains(t, out, "- a plain bullet with no link")
	// Trailing newline preserved.
	assert.True(t, len(out) > 0 && out[len(out)-1] == '\n')
}

func TestPruneIndexContent_NoChangeReturnsOriginal(t *testing.T) {
	dir := writeMemDir(t, "", "a.md", "b.md")
	content := "- [A](a.md) — x\n- [B](b.md) — y\n"

	out, removed := pruneIndexContent(content, dir, nil, true)
	assert.Nil(t, removed)
	assert.Equal(t, content, out) // byte-identical when nothing pruned
}

func TestPruneIndexContent_AlsoMissingSimulatesPendingDelete(t *testing.T) {
	// a.md exists on disk, but the caller is about to delete it.
	dir := writeMemDir(t, "", "a.md", "b.md")
	content := "- [A](a.md) — x\n- [B](b.md) — y\n"

	// treatMissingAsGone=false (the clean path): only alsoMissing drives
	// removal, even though a.md still exists on disk.
	out, removed := pruneIndexContent(content, dir, map[string]bool{"a.md": true}, false)
	require.Len(t, removed, 1)
	assert.Equal(t, "a.md", removed[0].target)
	assert.NotContains(t, out, "(a.md)")
	assert.Contains(t, out, "(b.md)")
}

func TestPruneIndexContent_NoTrailingNewlinePreserved(t *testing.T) {
	dir := writeMemDir(t, "", "keep.md")
	content := "- [Keep](keep.md) — x\n- [Gone](gone.md) — y" // no trailing \n

	out, removed := pruneIndexContent(content, dir, nil, true)
	require.Len(t, removed, 1)
	assert.Equal(t, "- [Keep](keep.md) — x", out) // no spurious trailing newline
}

func TestPruneIndexFile_WritesBackAndIsMissingSafe(t *testing.T) {
	dir := writeMemDir(t,
		"- [Keep](keep.md) — x\n- [Gone](gone.md) — y\n",
		"keep.md",
	)

	// Dry-run computes but does not write.
	removed, err := pruneIndexFile(dir, nil, true, true)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	onDisk, err := os.ReadFile(filepath.Join(dir, memoryIndexFile))
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "gone.md", "dry-run must not modify the file")

	// Real run writes the pruned content.
	removed, err = pruneIndexFile(dir, nil, true, false)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	onDisk, err = os.ReadFile(filepath.Join(dir, memoryIndexFile))
	require.NoError(t, err)
	assert.NotContains(t, string(onDisk), "gone.md")
	assert.Contains(t, string(onDisk), "keep.md")

	// A dir with no MEMORY.md is not an error.
	empty := t.TempDir()
	removed, err = pruneIndexFile(empty, nil, true, false)
	require.NoError(t, err)
	assert.Nil(t, removed)
}
