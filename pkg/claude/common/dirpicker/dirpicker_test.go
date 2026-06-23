package dirpicker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeChosenPath(t *testing.T) {
	cases := map[string]string{
		"/Users/me/dir/\n": "/Users/me/dir",
		"/Users/me/dir/":   "/Users/me/dir",
		"  /a/b  ":         "/a/b",
		"/":                "/",
		"":                 "",
		`C:\Users\me`:      `C:\Users\me`, // backslashes left intact
	}
	for in, want := range cases {
		if got := normalizeChosenPath(in); got != want {
			t.Errorf("normalizeChosenPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeStartDir(t *testing.T) {
	dir := t.TempDir()

	if got := sanitizeStartDir(dir); got != dir {
		t.Errorf("existing dir: got %q, want %q", got, dir)
	}
	if got := sanitizeStartDir("  " + dir + "  "); got != dir {
		t.Errorf("whitespace-padded dir: got %q, want %q", got, dir)
	}
	if got := sanitizeStartDir(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("missing dir: got %q, want \"\"", got)
	}
	if got := sanitizeStartDir(""); got != "" {
		t.Errorf("empty: got %q, want \"\"", got)
	}

	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sanitizeStartDir(f); got != "" {
		t.Errorf("regular file: got %q, want \"\"", got)
	}
}
