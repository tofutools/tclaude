package jstest

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	// agentd embeds the production dashboard tree. Keeping that package in this
	// test binary's dependency graph makes dashboard contents part of the build
	// ID, so Go's test cache is invalidated by content, not only file metadata.
	_ "github.com/tofutools/tclaude/pkg/claude/agentd"
)

// nodeTestInputs makes every suite, harness module, and nested vendor runtime
// part of the test binary's content-addressed build ID.
//
//go:embed *.mjs vendor
var nodeTestInputs embed.FS

// Dashboard pure-module and Preact component suites run with Node's
// built-in test runner. Preact suites use the committed test-only LinkeDOM
// runtime and exact dashboard modules, with no node_modules or install step.
// Keeping this wrapper in its own Go package lets the standard `go test ./...`
// scheduler overlap the JS suite with agentd's serial flow tests.
//
// Node availability:
//   - In CI the Test job runs actions/setup-node, so node is guaranteed.
//     A missing node there is a real failure; CI must not pass without running
//     the JS suites.
//   - Locally a node-less contributor skips this test, so they are not blocked.
func TestDashboardJS(t *testing.T) {
	ci := os.Getenv("CI") != ""
	nodeCacheKey := os.Getenv("TCLAUDE_NODE_TEST_CACHE_KEY")
	recordNodeInputs(t)

	node, err := exec.LookPath("node")
	if err != nil {
		if ci {
			t.Fatal("node not on PATH in CI — JS unit tests did not run " +
				"(the Test job is expected to run actions/setup-node)")
		}
		t.Skip("node not on PATH — skipping JS unit tests (install node to run them)")
	}
	if ci && nodeCacheKey == "" {
		t.Fatal("TCLAUDE_NODE_TEST_CACHE_KEY is empty in CI — the external Node runtime " +
			"must participate in Go's test-cache key")
	}
	if version, versionErr := exec.Command(node, "--version").Output(); versionErr == nil {
		t.Logf("Node runtime: %s (%s)", node, bytes.TrimSpace(version))
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
	args := []string{"--test"}
	if help, helpErr := exec.Command(node, "--help").Output(); helpErr == nil &&
		bytes.Contains(help, []byte("--test-concurrency")) {
		// Node runs one process per suite file, so serial execution pays a
		// full runtime start + module import per file (~90s across the whole
		// suite). Default to fanning files out across the spare cores, leaving
		// two for the Go packages (agentd's serial flow tests in particular)
		// that `go test -p` overlaps with this one. When this package has a
		// runner to itself — CI's jstest shard — that reservation would leave
		// the 3-core macOS runner effectively serial, so the shard overrides
		// with the full core count via TCLAUDE_JS_TEST_CONCURRENCY.
		concurrency := max(1, runtime.NumCPU()-2)
		if env := os.Getenv("TCLAUDE_JS_TEST_CONCURRENCY"); env != "" {
			parsed, parseErr := strconv.Atoi(env)
			if parseErr != nil || parsed < 1 {
				t.Fatalf("invalid TCLAUDE_JS_TEST_CONCURRENCY %q: %v", env, parseErr)
			}
			concurrency = parsed
		}
		args = append(args, fmt.Sprintf("--test-concurrency=%d", concurrency))
	}
	args = append(args, files...)
	out, err := exec.Command(node, args...).CombinedOutput()
	if err != nil {
		// A non-zero Node exit means the suite ran and failed. Preserve its output
		// so CI failures identify the actual assertion instead of looking like an
		// unavailable runtime.
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("dashboard JS unit tests failed: %v\n%s", err, out)
		}
		if ci {
			t.Fatal("node not runnable in CI — JS unit tests did not run "+
				"(the Test job is expected to run actions/setup-node)", err)
		}
		t.Skip("unable to run node — skipping JS unit tests (install node to run them)", err)
	}
	t.Logf("node %v:\n%s", args, out)
}

// recordNodeInputs hashes the runtime input set for diagnostics. nodeTestInputs
// and the agentd import above make those bytes content-addressed build inputs;
// the direct dashboard reads also expose their paths in Go's test-cache trace.
func recordNodeInputs(t *testing.T) {
	t.Helper()

	hash := sha256.New()
	fileCount := 0
	recordData := func(path string, data []byte) {
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		fileCount++
	}
	recordFile := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		recordData(path, data)
		return nil
	}
	recordTree := func(root string) error {
		return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			return recordFile(path)
		})
	}

	if err := recordTree("../dashboard"); err != nil {
		t.Fatalf("recording dashboard test inputs: %v", err)
	}
	if err := fs.WalkDir(nodeTestInputs, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := nodeTestInputs.ReadFile(path)
		if err != nil {
			return err
		}
		recordData(path, data)
		return nil
	}); err != nil {
		t.Fatalf("recording embedded Node test inputs: %v", err)
	}
	if fileCount == 0 {
		t.Fatal("no Node test inputs found")
	}
	t.Logf("Node test inputs: %d files, sha256 %x", fileCount, hash.Sum(nil))
}
