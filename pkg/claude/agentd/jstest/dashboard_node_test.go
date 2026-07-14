package jstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Dashboard pure-module and Preact component suites run with Node's
// built-in test runner. Preact suites use the committed test-only LinkeDOM
// runtime and exact dashboard modules, with no node_modules or install step.
// Keeping this wrapper in its own Go package lets the standard `go test -p 2
// ./...` scheduler overlap the JS suite with agentd's serial flow tests.
//
// Node availability:
//   - In CI the Test job runs actions/setup-node, so node is guaranteed.
//     A missing node there is a real failure; CI must not pass without running
//     the JS suites.
//   - Locally a node-less contributor skips this test, so they are not blocked.
func TestDashboardJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node not on PATH in CI — JS unit tests did not run " +
				"(the Test job is expected to run actions/setup-node)")
		}
		t.Skip("node not on PATH — skipping JS unit tests (install node to run them)")
	}

	// go test runs with this package directory as the working directory. Pass
	// every suite explicitly because Node does not treat a bare directory as a
	// positional test path.
	files, err := filepath.Glob("*.test.mjs")
	if err != nil {
		t.Fatalf("globbing *.test.mjs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *.test.mjs files found")
	}
	// Bound Node's file-level fan-out so this package can overlap agentd without
	// consuming every runner CPU and stretching both suites' critical path.
	args := append([]string{"--test", "--test-concurrency=2"}, files...)
	out, err := exec.Command(node, args...).CombinedOutput()
	if err != nil {
		// A non-zero Node exit means the suite ran and failed. Preserve its output
		// so CI failures identify the actual assertion instead of looking like an
		// unavailable runtime.
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("dashboard JS unit tests failed: %v\n%s", err, out)
		}
		if os.Getenv("CI") != "" {
			t.Fatal("node not runnable in CI — JS unit tests did not run "+
				"(the Test job is expected to run actions/setup-node)", err)
		}
		t.Skip("unable to run node — skipping JS unit tests (install node to run them)", err)
	}
	t.Logf("node %v:\n%s", args, out)
}
