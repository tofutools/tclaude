package memoryfiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

func TestRunPruneIndex_RemovesDanglingKeepsPresent(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "feedback_logging.md"},
	})
	writeIndex(t, projects, encoded, "# Memory Index\n\n"+
		"- [Logging](feedback_logging.md) — slog\n"+
		"- [Project: pappfigur](project_pappfigur.md) — deleted by hand\n")

	stdout := tmpStream(t, "")
	code := RunPruneIndex(&PruneIndexParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	require.Equal(t, 0, code)

	out := readStream(t, stdout)
	assert.Contains(t, out, "Dangling MEMORY.md 1 entry")
	assert.Contains(t, out, "Pruned 1 entry")

	got, err := os.ReadFile(filepath.Join(projects, encoded, "memory", "MEMORY.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(got), "project_pappfigur.md")
	assert.Contains(t, string(got), "feedback_logging.md")
}

func TestRunPruneIndex_NoDangling(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "a.md"},
	})
	writeIndex(t, projects, encoded, "- [A](a.md) — x\n")

	stdout := tmpStream(t, "")
	code := RunPruneIndex(&PruneIndexParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	require.Equal(t, 0, code)
	assert.Contains(t, readStream(t, stdout), "No dangling MEMORY.md entries")
}

func TestRunPruneIndex_DryRunDoesNotWrite(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{{encoded, "MEMORY.md"}})
	writeIndex(t, projects, encoded, "- [Gone](gone.md) — y\n")

	stdout := tmpStream(t, "")
	code := RunPruneIndex(&PruneIndexParams{Dir: target, DryRun: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	require.Equal(t, 0, code)

	out := readStream(t, stdout)
	assert.Contains(t, out, "gone.md")
	assert.Contains(t, out, "Dry run")

	got, _ := os.ReadFile(filepath.Join(projects, encoded, "memory", "MEMORY.md"))
	assert.Contains(t, string(got), "gone.md") // unchanged
}

func TestRunPruneIndex_ConfirmAborts(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{{encoded, "MEMORY.md"}})
	writeIndex(t, projects, encoded, "- [Gone](gone.md) — y\n")

	stdout := tmpStream(t, "")
	code := RunPruneIndex(&PruneIndexParams{Dir: target}, stdout, tmpStream(t, ""), tmpStream(t, "n\n"))
	require.Equal(t, 0, code)

	assert.Contains(t, readStream(t, stdout), "Aborted.")
	got, _ := os.ReadFile(filepath.Join(projects, encoded, "memory", "MEMORY.md"))
	assert.Contains(t, string(got), "gone.md") // unchanged
}

func TestRunPruneIndex_ConfirmYesPrunes(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{{encoded, "MEMORY.md"}})
	writeIndex(t, projects, encoded, "- [Gone](gone.md) — y\n- [Also](also_gone.md) — z\n")

	stdout := tmpStream(t, "")
	code := RunPruneIndex(&PruneIndexParams{Dir: target}, stdout, tmpStream(t, ""), tmpStream(t, "y\n"))
	require.Equal(t, 0, code)

	assert.Contains(t, readStream(t, stdout), "Pruned 2 entries")
	got, _ := os.ReadFile(filepath.Join(projects, encoded, "memory", "MEMORY.md"))
	assert.NotContains(t, string(got), "gone.md")
}

func TestRunPruneIndex_NoMatchingProjectDirs(t *testing.T) {
	target := "/work/proj"
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755))

	stdout := tmpStream(t, "")
	code := RunPruneIndex(&PruneIndexParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	require.Equal(t, 0, code)
	assert.Contains(t, readStream(t, stdout), "No Claude project directories found")
}
