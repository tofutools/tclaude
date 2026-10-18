package dashboard

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// The embed patterns are an explicit list, so a new kind of top-level asset
// (a .png, a fonts/ directory) would be missing from the served tree without
// any build error. Pin the list to the directory instead.
func TestFSCoversTheDirectory(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if _, err := fs.Stat(FS, name); err != nil {
			t.Errorf("%s is in the dashboard directory but not embedded; add it to the //go:embed patterns: %v", name, err)
		}
	}
	if _, err := fs.Stat(FS, "assets.go"); err == nil {
		t.Error("the package's own Go source is embedded and would be served as an asset")
	}
}
