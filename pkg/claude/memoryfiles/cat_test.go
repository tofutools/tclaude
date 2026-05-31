package memoryfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

func TestRunCat_IndexFirstThenAlphaTopLevelMDOnly(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	memEnv(t, target, []memFileSpec{
		{encoded, "aaa.md"},
		{encoded, "MEMORY.md"},
		{encoded, "zzz.md"},
		{encoded, "skip.txt"},    // non-md → not printed
		{encoded, ".idea/x.xml"}, // subdir → not printed
	})

	stdout := tmpStream(t, "")
	code := RunCat(&CatParams{Dir: target}, stdout, tmpStream(t, ""))
	assert.Equal(t, 0, code)
	out := readStream(t, stdout)

	// Contents of the three .md files are printed; the others are not.
	assert.Contains(t, out, "content of MEMORY.md")
	assert.Contains(t, out, "content of aaa.md")
	assert.Contains(t, out, "content of zzz.md")
	assert.NotContains(t, out, "skip.txt")
	assert.NotContains(t, out, ".idea")

	// MEMORY.md prints first (despite sorting after 'aaa' alphabetically),
	// then the rest alphabetically.
	iMem := strings.Index(out, "memory/MEMORY.md")
	iAaa := strings.Index(out, "memory/aaa.md")
	iZzz := strings.Index(out, "memory/zzz.md")
	require.True(t, iMem >= 0 && iAaa >= 0 && iZzz >= 0)
	assert.Less(t, iMem, iAaa, "MEMORY.md should print before aaa.md")
	assert.Less(t, iAaa, iZzz, "aaa.md should print before zzz.md")
}

func TestRunCat_NoSiblingsRestricts(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded + "-feature", "note.md"},
	})

	stdout := tmpStream(t, "")
	assert.Equal(t, 0, RunCat(&CatParams{Dir: target, NoSiblings: true}, stdout, tmpStream(t, "")))
	out := readStream(t, stdout)
	assert.Contains(t, out, "content of MEMORY.md")
	assert.NotContains(t, out, "-feature")
	assert.NotContains(t, out, "content of note.md")
}

func TestRunCat_NoMatchingProjectDirs(t *testing.T) {
	target := "/work/proj"
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755))

	stdout := tmpStream(t, "")
	assert.Equal(t, 0, RunCat(&CatParams{Dir: target}, stdout, tmpStream(t, "")))
	assert.Contains(t, strings.ToLower(readStream(t, stdout)), "no claude project directories found")
}
