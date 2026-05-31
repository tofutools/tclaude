package memoryfiles

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// initRepoWithWorktree creates, under an isolated $HOME, a real git repo
// at <home>/work/myrepo with one extra worktree at <home>/work/myrepo-feat.
// It returns the (symlink-resolved) home, repo, and worktree paths. The
// temp dir is symlink-resolved up front so the paths git reports match
// the ones we encode (macOS /var -> /private/var would otherwise differ).
func initRepoWithWorktree(t *testing.T) (home, repo, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home = base
	t.Setenv("HOME", home)
	// Keep the real user/system git config (hooks, gpgsign, etc.) out.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "no-global-gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, "no-system-gitconfig"))

	git := func(dir string, args ...string) {
		out, gErr := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoErrorf(t, gErr, "git %v: %s", args, out)
	}

	repo = filepath.Join(home, "work", "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(repo, "init", "-q")
	git(repo, "config", "user.email", "t@example.com")
	git(repo, "config", "user.name", "t")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644))
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "init")

	worktree = filepath.Join(home, "work", "myrepo-feat")
	git(repo, "worktree", "add", "-q", worktree)
	return home, repo, worktree
}

// seedMemory creates <projects>/<enc>/memory/MEMORY.md for each enc.
func seedMemory(t *testing.T, projects string, encs ...string) {
	t.Helper()
	for _, enc := range encs {
		dir := filepath.Join(projects, enc, "memory")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("m"), 0o644))
	}
}

func TestRunClean_WorktreesStrategyIsDefault(t *testing.T) {
	home, repo, worktree := initRepoWithWorktree(t)
	projects := filepath.Join(home, ".claude", "projects")

	encRepo := convops.PathToProjectDir(repo)
	encWt := convops.PathToProjectDir(worktree)
	encBak := encRepo + "-bak" // prefix-shaped, but NOT a git worktree

	seedMemory(t, projects, encRepo, encWt, encBak)

	// Default strategy = live git worktrees: deletes the repo's + its
	// worktree's memory, and leaves the prefix-shaped non-worktree dir.
	code := RunClean(&CleanParams{Dir: repo, Yes: true}, tmpStream(t, ""), tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)
	assert.False(t, exists(filepath.Join(projects, encRepo, "memory")), "repo memory should be deleted")
	assert.False(t, exists(filepath.Join(projects, encWt, "memory")), "worktree memory should be deleted")
	assert.True(t, exists(filepath.Join(projects, encBak, "memory", "MEMORY.md")), "non-worktree sibling must be untouched by the default strategy")
}

func TestRunClean_PrefixStrategySweepsNonWorktreeSiblings(t *testing.T) {
	home, repo, worktree := initRepoWithWorktree(t)
	projects := filepath.Join(home, ".claude", "projects")

	encRepo := convops.PathToProjectDir(repo)
	encWt := convops.PathToProjectDir(worktree)
	encBak := encRepo + "-bak"

	seedMemory(t, projects, encRepo, encWt, encBak)

	// --prefix sweeps every dir sharing the encoded prefix, including the
	// non-worktree -bak dir (the documented trade-off vs the default).
	code := RunClean(&CleanParams{Dir: repo, Prefix: true, Yes: true}, tmpStream(t, ""), tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)
	assert.False(t, exists(filepath.Join(projects, encRepo, "memory")))
	assert.False(t, exists(filepath.Join(projects, encWt, "memory")))
	assert.False(t, exists(filepath.Join(projects, encBak, "memory")), "--prefix should also remove the -bak sibling")
}

func TestRunClean_WorktreesIncludesExactSubdirTarget(t *testing.T) {
	home, repo, _ := initRepoWithWorktree(t)
	projects := filepath.Join(home, ".claude", "projects")

	sub := filepath.Join(repo, "frontend")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	encSub := convops.PathToProjectDir(sub)
	seedMemory(t, projects, encSub)

	// `git worktree list` only reports worktree ROOTS, so the subdir's own
	// memory dir (...-repo-frontend) is not among them. The default
	// strategy must still include the dir the user actually named, or it
	// would silently skip the memory for the location they're cleaning.
	code := RunClean(&CleanParams{Dir: sub, Yes: true}, tmpStream(t, ""), tmpStream(t, ""), tmpStream(t, ""))
	assert.Equal(t, 0, code)
	assert.False(t, exists(filepath.Join(projects, encSub, "memory")), "subdir target's own memory should be deleted, not skipped")
}

func TestRunClean_WorktreesFallbackWhenNotGitRepo(t *testing.T) {
	// A non-git target under an isolated HOME: the default strategy can't
	// list worktrees, so it falls back to the exact dir and emits a note.
	target := "/work/proj"
	encoded := convops.PathToProjectDir(target)
	projects := memEnv(t, target, []memFileSpec{
		{encoded, "MEMORY.md"},
		{encoded + "-feature", "MEMORY.md"}, // would match under --prefix, not here
	})

	stderr := tmpStream(t, "")
	code := RunClean(&CleanParams{Dir: target, Yes: true}, tmpStream(t, ""), stderr, tmpStream(t, ""))
	assert.Equal(t, 0, code)
	assert.Contains(t, readStream(t, stderr), "not a git worktree")
	assert.False(t, exists(filepath.Join(projects, encoded, "memory")), "exact dir cleaned")
	assert.True(t, exists(filepath.Join(projects, encoded+"-feature", "memory", "MEMORY.md")), "sibling untouched without --prefix")
}
