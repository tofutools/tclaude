package agentd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// defaultSiblingWorktreeTrust decides two things at once: whether a launch dir
// is auto-trusted, and (via the trust_dir_restricted refusals in lifecycle.go)
// whether an AGENT caller may request pre-trust for it at all. These unit tests
// pin the harness axis directly, which the flow tests cannot reach for a
// harness their simulator does not spawn.

// initSiblingWorktreeRepo builds a real repo plus a real linked worktree at the
// default sibling location (<repo>-<branch>) and returns both paths.
//
// The temp root is resolved through EvalSymlinks first — matching
// initRepoOnMain. IsDefaultSiblingWorktree compares cwd's parent against the
// parent of the git common dir, and `git rev-parse` reports a RESOLVED path, so
// on macOS (where t.TempDir() hands back /var/... for /private/var/...) an
// unresolved root makes the two disagree and the predicate wrongly returns
// false.
func initSiblingWorktreeRepo(t *testing.T) (repo, sibling string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "EvalSymlinks tempdir")
	repo = filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(repo, 0o755), "mkdir repo")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, gerr := cmd.CombinedOutput()
		require.NoError(t, gerr, "git %v: %s", args, out)
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "tclaude tests")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "init")
	// -M renames whatever the init default branch was (master / main) to main.
	run("branch", "-M", "main")

	sibling = filepath.Join(root, "proj-feature")
	run("worktree", "add", "-q", "-b", "feature", sibling)
	return repo, sibling
}

// A real default sibling worktree is auto-trusted for every harness that HAS a
// trust dialog, and for none that doesn't. OpenCode is the case that only the
// SupportsDirTrust gate rejects — a harness-name check would have had to name
// it explicitly, so this is what keeps the capability gate honest.
func TestDefaultSiblingWorktreeTrust_FollowsHarnessCapability(t *testing.T) {
	_, sibling := initSiblingWorktreeRepo(t)

	for _, tc := range []struct {
		harnessName string
		want        bool
	}{
		{harness.DefaultName, true},
		{harness.CodexName, true},
		{harness.OpenCodeName, false},
	} {
		t.Run(tc.harnessName, func(t *testing.T) {
			got, err := defaultSiblingWorktreeTrust(tc.harnessName, sibling, "")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got,
				"%s SupportsDirTrust=%v must decide sibling-worktree auto-trust",
				tc.harnessName, tc.want)
		})
	}
}

// The main worktree is not a SIBLING worktree, so it is never auto-trusted —
// pre-trusting a whole repo checkout is a decision only the operator makes.
func TestDefaultSiblingWorktreeTrust_MainWorktreeIsNotAutoTrusted(t *testing.T) {
	repo, _ := initSiblingWorktreeRepo(t)
	for _, name := range []string{harness.DefaultName, harness.CodexName} {
		got, err := defaultSiblingWorktreeTrust(name, repo, "")
		require.NoError(t, err)
		assert.False(t, got, "%s: the main worktree must not be auto-trusted", name)
	}
}

// A directory that is not a worktree at all resolves no Git common dir, so it
// falls through to false rather than erroring the spawn.
func TestDefaultSiblingWorktreeTrust_NonRepoDirIsNotAutoTrusted(t *testing.T) {
	got, err := defaultSiblingWorktreeTrust(harness.DefaultName, t.TempDir(), "")
	require.NoError(t, err)
	assert.False(t, got)
}
