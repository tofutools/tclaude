package harness

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestGitWorktreeWriteDirs(t *testing.T) {
	home := filepath.FromSlash("/home/dev")
	common := filepath.FromSlash("/home/dev/git/project/.git")
	want := []string{
		filepath.FromSlash("/home/dev/git"),
		filepath.FromSlash("/home/dev/git/project"),
		common,
	}
	if got := GitWorktreeWriteDirs(common, home); !reflect.DeepEqual(got, want) {
		t.Fatalf("GitWorktreeWriteDirs() = %v, want %v", got, want)
	}
}

func TestGitWorktreeWriteDirsDoesNotGrantHomeContainer(t *testing.T) {
	home := filepath.FromSlash("/home/dev")
	common := filepath.FromSlash("/home/dev/project/.git")
	want := []string{filepath.FromSlash("/home/dev/project"), common}
	if got := GitWorktreeWriteDirs(common, home); !reflect.DeepEqual(got, want) {
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
