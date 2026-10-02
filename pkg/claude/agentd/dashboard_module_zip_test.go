package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardAssetsSurviveModuleZip guards `go install
// github.com/tofutools/tclaude@latest`. The Go module zip format
// (golang.org/x/mod/zip) silently drops every file nested under a directory
// named "vendor" at any depth, so assets kept there embed fine from a
// checkout (and in goreleaser/brew builds) but are absent from a build out of
// the module proxy — the dashboard then 404s its xterm and Preact runtimes.
// Vendored browser assets live under dashboard/third_party/ instead.
func TestDashboardAssetsSurviveModuleZip(t *testing.T) {
	err := fs.WalkDir(dashboardFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, seg := range strings.Split(path, "/") {
			if seg == "vendor" {
				t.Errorf("embedded dashboard path %q sits under a vendor/ directory, which the Go module zip strips; use third_party/", path)
				return fs.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
