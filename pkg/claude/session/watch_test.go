package session

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
		completed, candidates := completeDirPath(filepath.Join(root, "project-b"))
		want := filepath.Join(root, "project-beta") + "/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("ambiguous match extends to common prefix and lists candidates", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "project-"))
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
		completed, candidates := completeDirPath(filepath.Join(root, "nope"))
		want := filepath.Join(root, "nope")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("files are not offered as directory completions", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "not-a"))
		want := filepath.Join(root, "not-a")
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
		}
		if candidates != nil {
			t.Errorf("candidates = %v, want nil", candidates)
		}
	})

	t.Run("trailing slash lists all subdirectories", func(t *testing.T) {
		completed, candidates := completeDirPath(filepath.Join(root, "project-alpha") + "/")
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
		completed, candidates := completeDirPath("~")
		want := home + "/"
		if completed != want {
			t.Errorf("completed = %q, want %q", completed, want)
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
		if got := expandHomePrefix(in); got != want {
			t.Errorf("expandHomePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
