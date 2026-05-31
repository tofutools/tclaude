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

// ---- classify / matchesAny (pure logic) ----

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		rel      string
		includes []string
		excludes []string
		wantDel  bool
	}{
		{"no filters deletes everything", "MEMORY.md", nil, nil, true},
		{"include match deletes", "feedback_x.md", []string{"feedback_*"}, nil, true},
		{"include miss keeps", "project_x.md", []string{"feedback_*"}, nil, false},
		{"exclude wins over default-all", "MEMORY.md", nil, []string{"MEMORY.md"}, false},
		{"exclude wins over include", "feedback_x.md", []string{"*.md"}, []string{"feedback_*"}, false},
		{"star matches any name", "note.md", []string{"*"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantDel, classify(tc.rel, tc.includes, tc.excludes))
		})
	}
}

// ---- resolveProjectDirs (sibling matching) ----

func TestResolveProjectDirs_ExactAndSiblings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".claude", "projects")
	require.NoError(t, os.MkdirAll(projects, 0o755))

	target := "/work/myproj"
	encoded := convops.PathToProjectDir(target) // "-work-myproj"

	// exact, two siblings, and a look-alike sibling that must NOT match
	for _, name := range []string{
		encoded,                // exact
		encoded + "-feature",   // worktree sibling
		encoded + "-joh-1-fix", // worktree sibling
		encoded + "2",          // distinct project "myproj2" — trailing-dash guard
		"-work-other",          // unrelated project
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(projects, name), 0o755))
	}
	// also a stray file (not a dir) sharing the prefix — must be ignored
	require.NoError(t, os.WriteFile(filepath.Join(projects, encoded+"-stray.txt"), []byte("x"), 0o644))

	withSiblings, err := resolveProjectDirs(target, scanPrefix)
	require.NoError(t, err)
	assert.Equal(t, encoded, withSiblings.encoded)
	assert.Equal(t, []string{
		filepath.Join(projects, encoded),
		filepath.Join(projects, encoded+"-feature"),
		filepath.Join(projects, encoded+"-joh-1-fix"),
	}, withSiblings.dirs)

	exactOnly, err := resolveProjectDirs(target, scanExact)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(projects, encoded)}, exactOnly.dirs)
}

func TestResolveProjectDirs_RejectsDegenerateTargetInPrefixMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755))

	// "/" encodes to "-"; matching siblings on "--" (or worse) would
	// sweep a huge chunk of ~/.claude/projects, so it must error out.
	_, err := resolveProjectDirs("/", scanPrefix)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "degenerate")
}

// ---- RunClean (end-to-end against an isolated $HOME) ----

// memEnv sets up an isolated $HOME with a Claude projects tree and
// returns the target dir + projects dir. Each memFileSpec is created as
// <projects>/<projectName>/memory/<rel>.
type memFileSpec struct {
	project string // encoded project dir name
	rel     string // path under memory/
}

func memEnv(t *testing.T, target string, specs []memFileSpec) (projects string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects = filepath.Join(home, ".claude", "projects")
	require.NoError(t, os.MkdirAll(projects, 0o755))
	for _, s := range specs {
		full := filepath.Join(projects, s.project, "memory", s.rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("content of "+s.rel), 0o644))
	}
	return projects
}

func tmpStream(t *testing.T, content string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stream")
	require.NoError(t, err)
	if content != "" {
		_, err = f.WriteString(content)
		require.NoError(t, err)
		_, err = f.Seek(0, 0)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func readStream(t *testing.T, f *os.File) string {
	t.Helper()
	require.NoError(t, f.Sync())
	b, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRunClean_DryRunDeletesNothing(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "feedback.md"},
	})

	stdout := tmpStream(t, "")
	stderr := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target, DryRun: true}, stdout, stderr, tmpStream(t, ""))

	assert.Equal(t, 0, code)
	out := readStream(t, stdout)
	assert.Contains(t, out, "Dry run")
	assert.Contains(t, out, "2 to delete, 0 to keep")
	// nothing removed
	assert.True(t, exists(filepath.Join(projects, encoded, "memory", "MEMORY.md")))
	assert.True(t, exists(filepath.Join(projects, encoded, "memory", "feedback.md")))
}

func TestRunClean_YesDeletesAllAndPrunesEmptyDir(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "feedback.md"},
	})

	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))

	assert.Equal(t, 0, code)
	out := readStream(t, stdout)
	assert.Contains(t, out, "Deleted 2 file(s)")
	assert.Contains(t, out, "removed 1 empty memory dir(s)")
	assert.False(t, exists(filepath.Join(projects, encoded, "memory")))
}

func TestRunClean_IncludeExcludeFilters(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "feedback_a.md"},
		{encoded, "feedback_b.md"},
		{encoded, "project_keep.md"},
	})

	// delete feedback_* but keep MEMORY.md explicitly
	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{
		Dir:     target,
		Include: []string{"feedback_*"},
		Exclude: []string{"MEMORY.md"},
		Yes:     true,
	}, stdout, tmpStream(t, ""), tmpStream(t, ""))

	assert.Equal(t, 0, code)
	memDir := filepath.Join(projects, encoded, "memory")
	assert.False(t, exists(filepath.Join(memDir, "feedback_a.md")))
	assert.False(t, exists(filepath.Join(memDir, "feedback_b.md")))
	assert.True(t, exists(filepath.Join(memDir, "MEMORY.md")))
	assert.True(t, exists(filepath.Join(memDir, "project_keep.md")))
	// dir not pruned because files remain
	assert.True(t, exists(memDir))
}

func TestRunClean_OnlyTopLevelMarkdownNoSubdirs(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded, "note.md"},
		{encoded, "README.txt"},            // non-.md → ignored entirely
		{encoded, ".idea/workspace.xml"},   // subdir → never traversed
		{encoded, ".idea/codeStyles/x.md"}, // .md but nested → still ignored
	})

	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)

	out := readStream(t, stdout)
	// Only the two top-level .md files are even mentioned.
	assert.Contains(t, out, "Deleted 2 file(s)")
	assert.NotContains(t, out, "README.txt")
	assert.NotContains(t, out, ".idea")

	memDir := filepath.Join(projects, encoded, "memory")
	assert.False(t, exists(filepath.Join(memDir, "MEMORY.md")))
	assert.False(t, exists(filepath.Join(memDir, "note.md")))
	// Untouched: non-md file and the whole subdir (incl. its nested .md).
	assert.True(t, exists(filepath.Join(memDir, "README.txt")))
	assert.True(t, exists(filepath.Join(memDir, ".idea", "workspace.xml")))
	assert.True(t, exists(filepath.Join(memDir, ".idea", "codeStyles", "x.md")))
	// memory/ dir not pruned because README.txt + .idea/ remain.
	assert.True(t, exists(memDir))
}

func TestRunClean_PrefixIncludesSiblings(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded + "-feature", "MEMORY.md"},
		{encoded + "2", "MEMORY.md"}, // distinct project, must be untouched
	})

	code := RunClean(&CleanParams{Dir: target, Prefix: true, Yes: true}, tmpStream(t, ""), tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)

	assert.False(t, exists(filepath.Join(projects, encoded, "memory")))
	assert.False(t, exists(filepath.Join(projects, encoded+"-feature", "memory")))
	// the distinct "proj2" project is left alone
	assert.True(t, exists(filepath.Join(projects, encoded+"2", "memory", "MEMORY.md")))
}

func TestRunClean_NoSiblingsRestrictsToExact(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded + "-feature", "MEMORY.md"},
	})

	code := RunClean(&CleanParams{Dir: target, NoSiblings: true, Yes: true}, tmpStream(t, ""), tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)

	assert.False(t, exists(filepath.Join(projects, encoded, "memory")))
	// sibling untouched
	assert.True(t, exists(filepath.Join(projects, encoded+"-feature", "memory", "MEMORY.md")))
}

func TestRunClean_ConfirmPromptAborts(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{{encoded, "MEMORY.md"}})

	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target}, stdout, tmpStream(t, ""), tmpStream(t, "n\n"))

	assert.Equal(t, 0, code)
	assert.Contains(t, readStream(t, stdout), "Aborted.")
	assert.True(t, exists(filepath.Join(projects, encoded, "memory", "MEMORY.md")))
}

func TestRunClean_ConfirmPromptYesDeletes(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{{encoded, "MEMORY.md"}})

	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target}, stdout, tmpStream(t, ""), tmpStream(t, "y\n"))

	assert.Equal(t, 0, code)
	assert.Contains(t, readStream(t, stdout), "Deleted 1 file(s)")
	assert.False(t, exists(filepath.Join(projects, encoded, "memory", "MEMORY.md")))
}

func TestRunClean_NoMemoryFiles(t *testing.T) {
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	// project dir exists but has no memory/ subdir
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".claude", "projects")
	require.NoError(t, os.MkdirAll(filepath.Join(projects, encoded), 0o755))

	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)
	assert.Contains(t, readStream(t, stdout), "No memory files found")
}

func TestRunClean_NoMatchingProjectDirs(t *testing.T) {
	target := "/work/proj"
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755))

	stdout := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target, Yes: true}, stdout, tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)
	assert.Contains(t, strings.ToLower(readStream(t, stdout)), "no claude project directories found")
}
