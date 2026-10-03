package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalTempDirThroughSymlink(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	var dir string
	t.Run("fixture", func(t *testing.T) {
		dir = CanonicalTempDir(t)
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(dir) || dir != resolved {
			t.Fatalf("got non-canonical path %q (resolved %q)", dir, resolved)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary directory survived subtest cleanup: %v", err)
	}
}
