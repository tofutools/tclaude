package memoryfiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

func TestRunLs_ListsTopLevelMarkdownOnly(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "note.md"},
		{encoded, "README.txt"},          // non-md → not listed
		{encoded, ".idea/workspace.xml"}, // subdir → not listed
	})

	stdout := tmpStream(t, "")
	code := RunLs(&LsParams{Dir: target}, stdout, tmpStream(t, ""))
	assert.Equal(t, 0, code)

	out := readStream(t, stdout)
	assert.Contains(t, out, "MEMORY.md")
	assert.Contains(t, out, "note.md")
	assert.NotContains(t, out, "README.txt")
	assert.NotContains(t, out, ".idea")
	assert.Contains(t, out, "Total: 2 file(s) across 1 project dir(s)")
}

func TestRunLs_IncludesSiblingsByDefault(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded + "-feature", "note.md"},
	})

	stdout := tmpStream(t, "")
	assert.Equal(t, 0, RunLs(&LsParams{Dir: target}, stdout, tmpStream(t, "")))
	assert.Contains(t, readStream(t, stdout), "Total: 2 file(s) across 2 project dir(s)")
}

func TestRunLs_NoSiblingsRestricts(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded + "-feature", "note.md"},
	})

	stdout := tmpStream(t, "")
	assert.Equal(t, 0, RunLs(&LsParams{Dir: target, NoSiblings: true}, stdout, tmpStream(t, "")))
	out := readStream(t, stdout)
	assert.Contains(t, out, "Total: 1 file(s) across 1 project dir(s)")
	assert.NotContains(t, out, "-feature")
}

func TestRunLs_NoMemoryFiles(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".claude", "projects")
	require.NoError(t, os.MkdirAll(filepath.Join(projects, encoded), 0o755)) // dir exists, no memory/

	stdout := tmpStream(t, "")
	assert.Equal(t, 0, RunLs(&LsParams{Dir: target}, stdout, tmpStream(t, "")))
	assert.Contains(t, readStream(t, stdout), "No memory files found")
}
