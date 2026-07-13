package agentd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The command palette's ranking logic lives in pure JS
// (dashboard/js/palette-score.js) and is unit-tested with Node's built-in
// test runner. Pure-module suites need no DOM; Preact component suites use the
// committed test-only LinkeDOM runtime and exact dashboard modules, still with
// no node_modules or install step. This wrapper runs every suite as part of
// `go test ./...` (the repo's single documented test entry point).
//
// node availability:
//   - In CI the Test job runs actions/setup-node, so node is guaranteed.
//     A missing node THERE is a real failure — we never let the suite
//     silently skip and let CI go green without having run (the classic
//     "green ≠ ran" trap). With this guard, a green CI provably means the
//     JS tests executed.
//   - Locally a node-less contributor just skips, so they aren't blocked.
//
// The .mjs imports the same raw ES module the browser loads; it lives in
// jstest/ (outside dashboard/) so `//go:embed dashboard` doesn't ship the
// test inside the agentd binary.
func TestPaletteScore_JS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node not on PATH in CI — JS unit tests did not run " +
				"(the Test job is expected to run actions/setup-node)")
		}
		t.Skip("node not on PATH — skipping JS unit tests (install node to run them)")
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
		// A started Node process that exits non-zero means the JS suite ran and
		// failed. Report its output in every environment, including CI; treating
		// this as "node not runnable" hid the actual flaky assertion on both CI
		// platforms and made the failure impossible to diagnose from job logs.
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("dashboard JS unit tests failed: %v\n%s", err, out)
		}
		if os.Getenv("CI") != "" {
			t.Fatal("node not runnable in CI — JS unit tests did not run "+
				"(the Test job is expected to run actions/setup-node)", err)
		}
		t.Skip("unable to run node — skipping JS unit tests (install node to run them)", err)
	}
	t.Logf("node --test %v:\n%s", files, out)
}
