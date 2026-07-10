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

func TestSandboxWorktreeContainer(t *testing.T) {
	home := filepath.FromSlash("/home/dev")

	// Normal linked-worktree layout: the container (parent of the main
	// worktree) is the grantable sandbox root, so it is returned.
	common := filepath.FromSlash("/home/dev/git/project/.git")
	want := filepath.FromSlash("/home/dev/git")
	if got := SandboxWorktreeContainer(common, home); got != want {
		t.Fatalf("SandboxWorktreeContainer() = %q, want %q", got, want)
	}

	// Home-guarded: the container would be home itself, so GitWorktreeWriteDirs
	// grants only the main worktree and there is no bare container to protect.
	homeContainer := filepath.FromSlash("/home/dev/project/.git")
	if got := SandboxWorktreeContainer(homeContainer, home); got != "" {
		t.Fatalf("SandboxWorktreeContainer() home-guarded = %q, want \"\"", got)
	}

	// A non-".git" common dir (bare repo / unusual layout) has no sibling
	// container to protect.
	if got := SandboxWorktreeContainer(filepath.FromSlash("/home/dev/git/bare.git"), home); got != "" {
		t.Fatalf("SandboxWorktreeContainer() non-.git = %q, want \"\"", got)
	}

	// Empty / relative input is a no-op.
	if got := SandboxWorktreeContainer("", home); got != "" {
		t.Fatalf("SandboxWorktreeContainer(\"\") = %q, want \"\"", got)
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
