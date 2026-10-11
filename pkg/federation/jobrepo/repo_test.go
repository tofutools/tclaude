package jobrepo

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/testutil"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(gitEnvironment(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}
func fixture(t *testing.T) (Definition, string, string) {
	t.Helper()
	root := testutil.CanonicalTempDir(t)
	clone := filepath.Join(root, "clone")
	if e := os.Mkdir(clone, 0700); e != nil {
		t.Fatal(e)
	}
	runGit(t, clone, "init", "-b", "main")
	if e := os.WriteFile(filepath.Join(clone, "hello"), []byte("first\n"), 0600); e != nil {
		t.Fatal(e)
	}
	// A declared executable filter must never run during checkout.
	if e := os.WriteFile(filepath.Join(clone, ".gitattributes"), []byte("hello filter=unsafe\n"), 0600); e != nil {
		t.Fatal(e)
	}
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "first")
	commit := runGit(t, clone, "rev-parse", "HEAD")
	u := (&url.URL{Scheme: "file", Path: clone}).String()
	d, e := Inspect(context.Background(), u, clone, []int64{1})
	if e != nil {
		t.Fatal(e)
	}
	return d, root, commit
}
func TestPrepareExactCommitAndNoLocalHooksOrFilters(t *testing.T) {
	d, root, commit := fixture(t)
	marker := filepath.Join(root, "executed")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if e := os.WriteFile(filepath.Join(d.GitDir, "hooks", "post-checkout"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	runGit(t, d.Clone, "config", "filter.unsafe.smudge", "touch '"+marker+"'")
	runGit(t, d.Clone, "config", "filter.unsafe.required", "true")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "filter.unsafe.smudge")
	t.Setenv("GIT_CONFIG_VALUE_0", "touch '"+marker+"'")
	c, e := Prepare(context.Background(), d, filepath.Join(root, "job"), "main")
	if e != nil {
		t.Fatal(e)
	}
	if c.Commit != commit {
		t.Fatalf("commit %s want %s", c.Commit, commit)
	}
	b, e := os.ReadFile(filepath.Join(c.Path, "hello"))
	if e != nil || string(b) != "first\n" {
		t.Fatalf("checkout content %q: %v", b, e)
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatalf("hook/filter ran: %v", e)
	}
	if runGit(t, c.Path, "rev-parse", "HEAD") != commit {
		t.Fatal("wrong checkout")
	}
	if _, e = os.Stat(filepath.Join(c.Path, ".git")); e != nil {
		t.Fatal(e)
	}
}
func TestPrepareRejectsReplacedCloneAndLeavesNoFailedRoot(t *testing.T) {
	d, root, _ := fixture(t)
	if e := os.Rename(d.GitDir, d.GitDir+".old"); e != nil {
		t.Fatal(e)
	}
	runGit(t, d.Clone, "init")
	if _, e := Prepare(context.Background(), d, filepath.Join(root, "job"), "main"); e == nil {
		t.Fatal("accepted replaced clone")
	}
	if _, e := os.Stat(filepath.Join(root, "job")); !os.IsNotExist(e) {
		t.Fatalf("failed root left behind: %v", e)
	}
}
func TestPrepareUnknownRefCleansRoot(t *testing.T) {
	d, root, _ := fixture(t)
	dest := filepath.Join(root, "job")
	if _, e := Prepare(context.Background(), d, dest, "missing"); e == nil {
		t.Fatal("accepted missing ref")
	}
	if _, e := os.Stat(dest); !os.IsNotExist(e) {
		t.Fatalf("failed root left behind: %v", e)
	}
}
func TestRejectUnsafeURLAndRef(t *testing.T) {
	for _, v := range []string{"ext::sh -c touch", "https://u:pass@example.test/x", "ssh://-oProxyCommand=evil/x", "ssh://u:pass@example.test/x", "http://example.test/x", "file:relative", "https://example.test/x%0Ay", "git@-evil:path"} {
		if ValidateURL(v) == nil {
			t.Errorf("accepted URL %q", v)
		}
	}
	for _, v := range []string{"https://example.test/x", "ssh://git@example.test/x", "git@example.test:x", "file:///tmp/example"} {
		if e := ValidateURL(v); e != nil {
			t.Errorf("URL %q: %v", v, e)
		}
	}
	for _, v := range []string{"--upload-pack=evil", "main:refs/heads/other", "HEAD~1", "main\n", "../main", "refs/heads/a..b"} {
		if _, e := NormalizeRef(context.Background(), v); e == nil {
			t.Errorf("accepted ref %q", v)
		}
	}
}

func TestPreparePinnedLocalCommitWhenFetchFails(t *testing.T) {
	d, root, commit := fixture(t)
	d.URL = (&url.URL{Scheme: "file", Path: filepath.Join(root, "unreachable")}).String()
	c, e := Prepare(context.Background(), d, filepath.Join(root, "job"), commit)
	if e != nil {
		t.Fatal(e)
	}
	if c.Commit != commit {
		t.Fatalf("unexpected checkout: %+v", c)
	}
	if _, e = os.Stat(filepath.Join(c.Path, ".git", "objects", "info", "alternates")); !os.IsNotExist(e) {
		t.Fatalf("checkout retains an alternate: %v", e)
	}
	// The checkout remains valid after its operator clone disappears.
	if e = os.RemoveAll(d.Clone); e != nil {
		t.Fatal(e)
	}
	if runGit(t, c.Path, "rev-parse", "HEAD") != commit {
		t.Fatal("wrong pinned checkout")
	}
	runGit(t, c.Path, "fsck", "--full")
}
func TestPrepareNeverFallsBackToLocalBranch(t *testing.T) {
	d, root, _ := fixture(t)
	d.URL = (&url.URL{Scheme: "file", Path: filepath.Join(root, "unreachable")}).String()
	if _, e := Prepare(context.Background(), d, filepath.Join(root, "job"), "main"); e == nil {
		t.Fatal("silently used stale local branch")
	}
}

func TestPrepareReportsLocalResolutionAfterFetchFailure(t *testing.T) {
	d, root, commit := fixture(t)
	real, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	bin := filepath.Join(root, "bin")
	if e = os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	script := "#!/bin/sh\nfor arg do [ \"$arg\" != fetch ] || exit 1; done\nexec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if e = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d.URL = (&url.URL{Scheme: "file", Path: filepath.Join(root, "unreachable")}).String()
	c, e := Prepare(context.Background(), d, filepath.Join(root, "job"), commit)
	if e != nil {
		t.Fatal(e)
	}
	if c.Resolution != "resolved from local clone (fetch failed)" {
		t.Fatalf("missing fallback provenance: %+v", c)
	}
}
func TestPrepareReusesLinkedWorktreeCommonObjects(t *testing.T) {
	d, root, commit := fixture(t)
	linked := filepath.Join(root, "linked")
	runGit(t, d.Clone, "worktree", "add", "--detach", linked, commit)
	d, e := Inspect(context.Background(), d.URL, linked, []int64{1})
	if e != nil {
		t.Fatal(e)
	}
	c, e := Prepare(context.Background(), d, filepath.Join(root, "job"), commit)
	if e != nil {
		t.Fatal(e)
	}
	if c.Commit != commit {
		t.Fatal("wrong linked-worktree commit")
	}
}

func TestPreparePinnedShallowCloneWithoutRemote(t *testing.T) {
	d, root, _ := fixture(t)
	if e := os.WriteFile(filepath.Join(d.Clone, "second"), []byte("next\n"), 0600); e != nil {
		t.Fatal(e)
	}
	runGit(t, d.Clone, "add", "second")
	runGit(t, d.Clone, "commit", "-m", "second")
	commit := runGit(t, d.Clone, "rev-parse", "HEAD")
	shallow := filepath.Join(root, "shallow")
	runGit(t, root, "clone", "--depth=1", d.URL, shallow)
	d, e := Inspect(context.Background(), (&url.URL{Scheme: "file", Path: filepath.Join(root, "unreachable")}).String(), shallow, []int64{1})
	if e != nil {
		t.Fatal(e)
	}
	c, e := Prepare(context.Background(), d, filepath.Join(root, "job"), commit)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(shallow); e != nil {
		t.Fatal(e)
	}
	if runGit(t, c.Path, "rev-parse", "HEAD") != commit {
		t.Fatal("wrong shallow checkout")
	}
	if runGit(t, c.Path, "rev-parse", "--is-shallow-repository") != "true" {
		t.Fatal("lost shallow boundary")
	}
	runGit(t, c.Path, "fsck", "--full")
}

func TestFullCommitFanoutPins(t *testing.T) {
	for _, ref := range []string{strings.Repeat("a", 40), strings.Repeat("F", 64)} {
		if !FullCommit(ref) {
			t.Fatalf("full commit rejected: %q", ref)
		}
	}
	for _, ref := range []string{"main", "origin/main", strings.Repeat("a", 39), strings.Repeat("g", 40)} {
		if FullCommit(ref) {
			t.Fatalf("non-commit accepted: %q", ref)
		}
	}
}

func TestReceiverHeadAndSafeOriginHint(t *testing.T) {
	d, _, commit := fixture(t)
	head, err := Head(context.Background(), d)
	if err != nil || head != commit {
		t.Fatalf("HEAD %q: %v", head, err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"https://example.test/team/project.git", "https://example.test/team/project.git"},
		{"git@example.test:team/project.git", "git@example.test:team/project.git"},
		{"ssh://git@example.test/team/project.git", "ssh://git@example.test/team/project.git"},
		{"https://user:password@example.test/team/project.git", ""},
		{"https://token@example.test/team/project.git", ""},
		{"ssh://git:secret@example.test/team/project.git", ""},
		{"file:///source/private/checkout", ""},
		{"ext::unsafe helper", ""},
	} {
		runGit(t, d.Clone, "config", "remote.origin.url", tc.raw)
		if got := OriginHint(context.Background(), d.Clone); got != tc.want {
			t.Errorf("hint %q: got %q", tc.raw, got)
		}
	}
}
