//go:build linux || darwin

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingViewerReadsHaveNoLockSideEffects(t *testing.T) {
	root := t.TempDir()
	fs, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("run", func(t *testing.T) {
		const id = "missing-run"
		key := root + "\x00" + id
		if _, exists := processLocks.Load(key); exists {
			t.Fatal("unexpected preexisting run lock-map entry")
		}
		if _, err := fs.LoadRunView(t.Context(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("LoadRunView error = %v, want ErrNotFound", err)
		}
		if _, exists := processLocks.Load(key); exists {
			t.Fatal("missing run read created a lock-map entry")
		}
		if _, err := os.Stat(filepath.Join(root, ".locks", id+".lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing run lock stat error = %v, want not exist", err)
		}
	})

	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name  string
		id    string
		setup func(string)
	}{
		{name: "template id", id: "missing-template-id", setup: func(string) {}},
		{name: "template version", id: "missing-template-version", setup: func(id string) {
			if err := os.MkdirAll(filepath.Join(root, "templates", id), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "template body", id: "missing-template-body", setup: func(id string) {
			if err := os.MkdirAll(filepath.Join(root, "templates", id, "sha256-"+hash), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(tc.id)
			key := root + "\x00template\x00" + tc.id
			if _, exists := processLocks.Load(key); exists {
				t.Fatal("unexpected preexisting template lock-map entry")
			}
			ref := tc.id + "@sha256:" + hash
			if _, err := fs.GetTemplateExact(t.Context(), ref); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetTemplateExact error = %v, want ErrNotFound", err)
			}
			if _, exists := processLocks.Load(key); exists {
				t.Fatal("missing template read created a lock-map entry")
			}
			if _, err := os.Stat(filepath.Join(root, ".locks", "template-"+tc.id+".lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing template lock stat error = %v, want not exist", err)
			}
		})
	}
}
