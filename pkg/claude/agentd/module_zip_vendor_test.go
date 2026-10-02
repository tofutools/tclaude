package agentd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoVendorDirsInModule guards `go install
// github.com/tofutools/tclaude@latest`. The Go module zip format
// (golang.org/x/mod/zip) silently drops files under a directory named
// "vendor", so assets kept there embed fine from a checkout (and in
// goreleaser/brew builds) but are absent from a build out of the module proxy
// — the dashboard then 404s its xterm and Preact runtimes, and test packages
// embedding them fail to build from the module cache. tclaude does not use Go
// vendoring, so no directory in the module may be named vendor; vendored
// browser and test assets live under third_party/ instead.
func TestNoVendorDirsInModule(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch name := d.Name(); {
		case path != root && strings.HasPrefix(name, "."):
			// Hidden dirs (.git, editor/agent state) are not module content.
			return fs.SkipDir
		case name == "vendor":
			rel, _ := filepath.Rel(root, path)
			t.Errorf("directory %q is named vendor; the Go module zip strips its contents, so rename it (e.g. third_party/)", rel)
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
