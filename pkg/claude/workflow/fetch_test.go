package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── dir: source ─────────────────────────────────────────────────────────────

func TestResolveDir_Absolute(t *testing.T) {
	parent := t.TempDir()
	writeTemplate(t, parent, "mytmpl", "dir-wf")
	dir := filepath.Join(parent, "mytmpl")

	tmpl, err := Resolve("dir:" + dir)
	require.NoError(t, err)
	assert.Equal(t, "dir-wf", tmpl.Name)
	assert.Equal(t, SourceDir, tmpl.Source)
	assert.True(t, tmpl.Source.IsExternal())
	assert.Equal(t, dir, tmpl.Dir)
	assert.Equal(t, "dir:"+dir, tmpl.Ref)
}

func TestResolveDir_Relative(t *testing.T) {
	parent := t.TempDir()
	writeTemplate(t, parent, "rel", "rel-wf")
	t.Chdir(parent) // resolve "dir:rel" against this cwd

	tmpl, err := Resolve("dir:rel")
	require.NoError(t, err)
	assert.Equal(t, "rel-wf", tmpl.Name)
	assert.Equal(t, SourceDir, tmpl.Source)
}

func TestResolveDir_NotATemplate(t *testing.T) {
	_, err := Resolve("dir:" + t.TempDir()) // empty dir, no workflow.yaml
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a template dir")
}

func TestResolveDir_Empty(t *testing.T) {
	_, err := Resolve("dir:")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty directory path")
}

// ── git: spec parsing ───────────────────────────────────────────────────────

func TestParseGitSpec(t *testing.T) {
	cases := []struct {
		in            string
		url, ref, sub string
	}{
		{"git:https://h/o/r.git", "https://h/o/r.git", "", ""},
		{"git:https://h/o/r.git@main", "https://h/o/r.git", "main", ""},
		{"git:https://h/o/r.git@v1.2.3#sub/dir", "https://h/o/r.git", "v1.2.3", "sub/dir"},
		{"git:https://h/o/r@feature/x#t", "https://h/o/r", "feature/x", "t"}, // ref contains '/'
		{"git:https://user@host/o/r", "https://user@host/o/r", "", ""},       // userinfo, no ref
		{"git:https://user@host/o/r@main", "https://user@host/o/r", "main", ""},
		{"git:git@github.com:o/r.git", "git@github.com:o/r.git", "", ""}, // scp-style, no ref
		{"git:git@github.com:o/r.git@v1#t", "git@github.com:o/r.git", "v1", "t"},
		{"git:ssh://git@host/o/r.git@dev", "ssh://git@host/o/r.git", "dev", ""},
		{"git:/local/path/repo@main#t", "/local/path/repo", "main", "t"}, // local path
		{"git:/local/path/repo#t", "/local/path/repo", "", "t"},
		{"git:/local/path/repo", "/local/path/repo", "", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			gs, err := parseGitSpec(c.in)
			require.NoError(t, err)
			assert.Equal(t, c.url, gs.url, "url")
			assert.Equal(t, c.ref, gs.ref, "ref")
			assert.Equal(t, c.sub, gs.subpath, "subpath")
		})
	}
}

func TestParseGitSpec_Errors(t *testing.T) {
	for _, in := range []string{
		"git:", "git:   ", "git:@main#t", "git:#onlysub",
		"git:--upload-pack=evil",       // leading-dash url → flag injection
		"git:https://h/r@-evilref#sub", // leading-dash ref → flag injection
	} {
		t.Run(in, func(t *testing.T) {
			_, err := parseGitSpec(in)
			assert.Error(t, err)
		})
	}
}

func TestIsCommitSHA(t *testing.T) {
	assert.True(t, isCommitSHA("0123456789abcdef0123456789abcdef01234567")) // full 40 hex
	assert.False(t, isCommitSHA("abc1234"))                                 // abbreviated → mutable
	assert.False(t, isCommitSHA("main"))
	assert.False(t, isCommitSHA("v1.2.3"))
	assert.False(t, isCommitSHA("deadbeef"))                                  // hex-looking branch name
	assert.False(t, isCommitSHA("0123456789abcdef0123456789abcdef0123456g"))  // 40 chars, non-hex
	assert.False(t, isCommitSHA("feature/x"))
}

func TestCheckSubpath(t *testing.T) {
	for _, ok := range []string{"", "tmpl", "a/b/c", "./tmpl", "a/../b"} {
		assert.NoError(t, checkSubpath(ok), "expected %q to be allowed", ok)
	}
	for _, bad := range []string{"../etc", "a/../../etc", `..\..\etc`, "/abs", `C:\win`, ".."} {
		assert.Error(t, checkSubpath(bad), "expected %q to be rejected", bad)
	}
}

// ── git: resolution against a local bare-repo fixture (no network) ──────────

// gitFixture builds a local bare git repo containing a template under "tmpl/"
// and returns its path plus the SHA of the first commit (tagged v1.0.0). The
// "main" branch advances one commit past the tag/first-commit so branch ≠ tag.
func gitFixture(t *testing.T) (bareRepo, firstCommitSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))

	run := func(dir string, args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return string(out)
	}

	run(work, "init", "-q", "-b", "main")
	writeTemplate(t, work, "tmpl", "git-wf") // template at subpath tmpl/
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "initial")
	firstCommitSHA = strings.TrimSpace(run(work, "rev-parse", "HEAD"))
	run(work, "tag", "v1.0.0")
	// advance main past the tag so branch and tag point at different commits
	require.NoError(t, os.WriteFile(filepath.Join(work, "tmpl", "extra.txt"), []byte("x"), 0o644))
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "second")

	bareRepo = filepath.Join(root, "bare.git")
	run(root, "clone", "-q", "--bare", work, bareRepo)
	return bareRepo, firstCommitSHA
}

func TestResolveGit_Branch(t *testing.T) {
	bare, _ := gitFixture(t)
	tmpl, err := ResolveOpts("git:"+bare+"@main#tmpl", ResolveOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, "git-wf", tmpl.Name)
	assert.Equal(t, SourceGit, tmpl.Source)
	assert.True(t, tmpl.Source.IsExternal())
}

func TestResolveGit_Tag(t *testing.T) {
	bare, _ := gitFixture(t)
	tmpl, err := ResolveOpts("git:"+bare+"@v1.0.0#tmpl", ResolveOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, "git-wf", tmpl.Name)
}

func TestResolveGit_Commit(t *testing.T) {
	bare, sha := gitFixture(t)
	tmpl, err := ResolveOpts("git:"+bare+"@"+sha+"#tmpl", ResolveOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, "git-wf", tmpl.Name)
}

func TestResolveGit_DefaultBranch(t *testing.T) {
	bare, _ := gitFixture(t)
	tmpl, err := ResolveOpts("git:"+bare+"#tmpl", ResolveOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, "git-wf", tmpl.Name)
}

func TestResolveGit_RepoRootSubpath(t *testing.T) {
	// A template living at the repo root (no #subpath).
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))
	writeTemplate(t, work, ".", "root-wf") // template files directly in work/
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = work
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := c.CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
	}
	run("init", "-q", "-b", "main")
	run("add", "-A")
	run("commit", "-q", "-m", "init")

	tmpl, err := ResolveOpts("git:"+work+"@main", ResolveOptions{CacheDir: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, "root-wf", tmpl.Name)
}

func TestResolveGit_CacheHit_NoRefetch(t *testing.T) {
	bare, _ := gitFixture(t)
	cache := t.TempDir()
	ref := "git:" + bare + "#tmpl" // default branch (mutable), fresh stamp

	tmpl, err := ResolveOpts(ref, ResolveOptions{CacheDir: cache})
	require.NoError(t, err)
	assert.Equal(t, "git-wf", tmpl.Name)

	// Remove the source: a second resolve must succeed purely from cache (the
	// stamp is fresh, so no re-fetch is attempted).
	require.NoError(t, os.RemoveAll(bare))
	tmpl2, err := ResolveOpts(ref, ResolveOptions{CacheDir: cache})
	require.NoError(t, err, "expected a cache hit without re-cloning")
	assert.Equal(t, "git-wf", tmpl2.Name)
}

func TestResolveGit_CommitImmutable_NoRefetchEvenWhenStale(t *testing.T) {
	bare, sha := gitFixture(t)
	cache := t.TempDir()
	ref := "git:" + bare + "@" + sha + "#tmpl"

	_, err := ResolveOpts(ref, ResolveOptions{CacheDir: cache})
	require.NoError(t, err)

	// Even with an effectively-zero TTL, a pinned commit is immutable → cache
	// hit. Remove the source to prove no re-clone is attempted.
	require.NoError(t, os.RemoveAll(bare))
	_, err = ResolveOpts(ref, ResolveOptions{CacheDir: cache, TTL: 1})
	require.NoError(t, err, "a pinned commit must never re-fetch")
}

func TestResolveGit_Refresh_Refetches(t *testing.T) {
	bare, _ := gitFixture(t)
	cache := t.TempDir()
	ref := "git:" + bare + "@main#tmpl"

	_, err := ResolveOpts(ref, ResolveOptions{CacheDir: cache})
	require.NoError(t, err)

	// Forced refresh with the source still present → re-clone succeeds.
	_, err = ResolveOpts(ref, ResolveOptions{CacheDir: cache, Refresh: true})
	require.NoError(t, err)

	// Forced refresh after removing the source → re-clone must fail.
	require.NoError(t, os.RemoveAll(bare))
	_, err = ResolveOpts(ref, ResolveOptions{CacheDir: cache, Refresh: true})
	require.Error(t, err, "refresh after source removal should fail to re-clone")
}

func TestResolveGit_SubpathTraversalRejected(t *testing.T) {
	_, err := ResolveOpts("git:/some/repo@main#../../etc", ResolveOptions{CacheDir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes")
}

func TestResolveGit_MissingTemplateDir(t *testing.T) {
	bare, _ := gitFixture(t)
	_, err := ResolveOpts("git:"+bare+"@main#nonexistent", ResolveOptions{CacheDir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a template dir")
}

func TestCacheKey_StableAndRefDependent(t *testing.T) {
	a := cacheKey("https://h/o/r.git", "main")
	b := cacheKey("https://h/o/r.git", "main")
	c := cacheKey("https://h/o/r.git", "dev")
	assert.Equal(t, a, b, "same url+ref → same key")
	assert.NotEqual(t, a, c, "different ref → different key")
	assert.Contains(t, a, "r-", "key carries a readable repo-base prefix")
}
