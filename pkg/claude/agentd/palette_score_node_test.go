package agentd

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The command palette's ranking logic lives in pure JS
// (dashboard/js/palette-score.js) and is unit-tested with Node's built-in
// test runner — no bundler, no framework, no node_modules, matching the
// dashboard's deliberate no-build-system convention. This wrapper runs
// that suite as part of `go test ./...` (the repo's single documented test
// entry point) and SKIPS cleanly when node isn't installed, so a node-less
// contributor isn't blocked while CI — whose GitHub runners ship node —
// still gets the coverage.
//
// The .mjs imports the same raw ES module the browser loads; it lives in
// jstest/ (outside dashboard/) so `//go:embed dashboard` doesn't ship the
// test inside the agentd binary.
func TestPaletteScore_JS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — skipping JS unit tests (CI runners have node)")
	}
	// `go test` runs with the package dir as the working directory, so the
	// jstest/ glob resolves relative to it. Pass each *.test.mjs explicitly
	// (node's positional path handling doesn't treat a bare dir as a test
	// directory) — globbing keeps every suite picked up as the set grows.
	files, err := filepath.Glob("jstest/*.test.mjs")
	if err != nil {
		t.Fatalf("globbing jstest/*.test.mjs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no jstest/*.test.mjs files found")
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("palette-score JS unit tests failed: %v\n%s", err, out)
	}
	t.Logf("node --test %v:\n%s", files, out)
}
