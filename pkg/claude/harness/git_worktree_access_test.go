package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGitWorktreeWriteDirs(t *testing.T) {
	home := filepath.FromSlash("/home/dev")
	common := filepath.FromSlash("/home/dev/git/project/.git")
	want := []string{filepath.FromSlash("/home/dev/git")}
	if got := GitWorktreeWriteDirs(common, home); !reflect.DeepEqual(got, want) {
		t.Fatalf("GitWorktreeWriteDirs() = %v, want %v", got, want)
	}
}

func TestGitWorktreeWriteDirsDoesNotGrantHomeContainer(t *testing.T) {
	home := filepath.FromSlash("/home/dev")
	common := filepath.FromSlash("/home/dev/project/.git")
	want := []string{filepath.FromSlash("/home/dev/project")}
	if got := GitWorktreeWriteDirs(common, home); !reflect.DeepEqual(got, want) {
		t.Fatalf("GitWorktreeWriteDirs() = %v, want %v", got, want)
	}
}

func TestGitWorktreeWriteDirsCanonicalizesHomeAlias(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(home, "project")
	common := filepath.Join(repo, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	homeAlias := filepath.Join(root, "home-alias")
	if err := os.Symlink(home, homeAlias); err != nil {
		t.Fatal(err)
	}
	resolvedCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Dir(resolvedCommon)}
	if got := GitWorktreeWriteDirs(resolvedCommon, homeAlias); !reflect.DeepEqual(got, want) {
		t.Fatalf("GitWorktreeWriteDirs() = %v, want %v", got, want)
	}
}

func TestIsDefaultSiblingWorktree(t *testing.T) {
	common := filepath.FromSlash("/home/dev/git/project/.git")
	for _, tc := range []struct {
		cwd  string
		want bool
	}{
		{filepath.FromSlash("/home/dev/git/project-feature"), true},
		{filepath.FromSlash("/home/dev/git/project"), false},
		{filepath.FromSlash("/home/dev/git/other-feature"), false},
		{filepath.FromSlash("/home/dev/git/project-feature/subdir"), false},
	} {
		if got := IsDefaultSiblingWorktree(tc.cwd, common); got != tc.want {
			t.Errorf("IsDefaultSiblingWorktree(%q) = %v, want %v", tc.cwd, got, tc.want)
		}
	}
}
