package agentd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Mirrors process_graph_node_test.go: the worklist sub-view has no build
// step, so its pure view/filter/format core is exercised under plain Node
// (skipped locally when node is absent, fatal in CI).

func processWorklistNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	if os.Getenv("CI") != "" {
		t.Fatal("node not on PATH in CI — process worklist JS tests did not run")
	}
	t.Skip("node not on PATH — skipping process worklist JS tests")
	return ""
}

// Syntax-check each shipped worklist module as an ES module, matching the
// dashboard's native-module loading mode.
func TestProcessWorklist_JSSyntax(t *testing.T) {
	node := processWorklistNode(t)
	for _, file := range []string{
		"dashboard/js/process-worklist-core.js",
		"dashboard/js/process-worklist.js",
		"dashboard/js/processes.js",
	} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		cmd := exec.Command(node, "--input-type=module", "--check")
		cmd.Stdin = bytes.NewReader(source)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node --input-type=module --check %s: %v\n%s", file, err, out)
		}
	}
}

func TestProcessWorklist_CoreJS(t *testing.T) {
	node := processWorklistNode(t)
	file := filepath.Join("jstest", "process-worklist.test.mjs")
	out, err := exec.Command(node, "--test", file).CombinedOutput()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("process worklist JS unit tests failed: %v\n%s", err, out)
		}
		if os.Getenv("CI") != "" {
			t.Fatalf("node not runnable in CI: %v", err)
		}
		t.Skipf("unable to run node: %v", err)
	}
	t.Logf("node --test %s:\n%s", file, out)
}
