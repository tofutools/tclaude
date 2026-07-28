package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompleteDirPath(t *testing.T) {
	root := t.TempDir()
	mustMkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir("project-alpha")
	mustMkdir("project-beta")
	mustMkdir("project-alpha/sub")
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("unambiguous match completes with trailing slash", func(t *testing.T) {
		completed, candidates := CompleteDirPath(filepath.Join(root, "project-b"))
		want := filepath.Join(root, "project-beta") + "/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("ambiguous match extends to common prefix and lists candidates", func(t *testing.T) {
		completed, candidates := CompleteDirPath(filepath.Join(root, "project-"))
		want := filepath.Join(root, "project-")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		wantCandidates := []string{"project-alpha", "project-beta"}
		if len(candidates) != len(wantCandidates) || candidates[0] != wantCandidates[0] || candidates[1] != wantCandidates[1] {
			t.Errorf("candidates = %v, want %v", candidates, wantCandidates)
		}
	})

	t.Run("no match leaves input unchanged", func(t *testing.T) {
		completed, candidates := CompleteDirPath(filepath.Join(root, "nope"))
		want := filepath.Join(root, "nope")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("files are not offered as directory completions", func(t *testing.T) {
		completed, candidates := CompleteDirPath(filepath.Join(root, "not-a"))
		want := filepath.Join(root, "not-a")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("trailing slash lists all subdirectories", func(t *testing.T) {
		completed, candidates := CompleteDirPath(filepath.Join(root, "project-alpha") + "/")
		want := filepath.Join(root, "project-alpha") + "/sub/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("bare tilde completes to home with trailing slash", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home directory available")
		}
		completed, candidates := CompleteDirPath("~")
		want := home + "/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})
}

// "~/" means "list my home directory", the same as any other path ending in
// a separator. Home expansion runs through filepath.Join, which cleans that
// separator off, and the completion below used to read the resulting last
// segment (the home directory's own basename) as the segment being
// completed — slicing out of range and panicking in the middle of a TUI
// update loop, which costs the operator their console.
func TestCompleteDirPathListsHomeChildren(t *testing.T) {
	root := t.TempDir()

	t.Run("several children list as candidates", func(t *testing.T) {
		home := filepath.Join(root, "home-many")
		for _, name := range []string{"alpha", "beta"} {
			if err := os.MkdirAll(filepath.Join(home, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("HOME", home)

		completed, candidates := CompleteDirPath("~/")
		if completed != "~/" {
			t.Errorf("completed = %q, want %q", completed, "~/")
		}
		want := []string{"alpha", "beta"}
		if len(candidates) != len(want) || candidates[0] != want[0] || candidates[1] != want[1] {
			t.Errorf("candidates = %v, want %v", candidates, want)
		}
	})

	t.Run("a single child completes without resolving the tilde", func(t *testing.T) {
		home := filepath.Join(root, "home-one")
		if err := os.MkdirAll(filepath.Join(home, "only"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)

		completed, candidates := CompleteDirPath("~/")
		if completed != "~/only/" {
			t.Errorf("completed = %q, want %q", completed, "~/only/")
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("a partial home-relative segment still completes", func(t *testing.T) {
		home := filepath.Join(root, "home-partial")
		if err := os.MkdirAll(filepath.Join(home, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)

		completed, candidates := CompleteDirPath("~/work")
		if completed != "~/workspace/" {
			t.Errorf("completed = %q, want %q", completed, "~/workspace/")
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})
}

func TestExpandHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}

	cases := map[string]string{
		"~":        home,
		"~/foo":    filepath.Join(home, "foo"),
		"/etc":     "/etc",
		"relative": "relative",
		"~foo/bar": "~foo/bar", // not a home-relative path (no leading "~/")
	}
	for in, want := range cases {
		if got := ExpandHomePrefix(in); got != want {
			t.Errorf("ExpandHomePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
