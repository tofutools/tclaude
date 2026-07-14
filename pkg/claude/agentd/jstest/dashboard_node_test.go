package jstest

import (
	"crypto/sha256"
	"errors"
	"io/fs"
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
	recordDashboardInputs(t)

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
	// Keep Node's file-level fan-out serial so this package can overlap agentd
	// without competing for runner CPUs and stretching both suites' critical path.
	args := append([]string{"--test", "--test-concurrency=1"}, files...)
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

// recordDashboardInputs makes production dashboard assets explicit inputs to
// Go's test cache. The cache observes files opened by this process, not files
// that the Node child opens, so without this read a dashboard-only edit could
// incorrectly reuse a cached successful Node run.
func recordDashboardInputs(t *testing.T) {
	t.Helper()

	hash := sha256.New()
	fileCount := 0
	err := filepath.WalkDir("../dashboard", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		fileCount++
		return nil
	})
	if err != nil {
		t.Fatalf("recording dashboard test inputs: %v", err)
	}
	if fileCount == 0 {
		t.Fatal("no dashboard test inputs found")
	}
	t.Logf("dashboard test inputs: %d files, sha256 %x", fileCount, hash.Sum(nil))
}
