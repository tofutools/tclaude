package agentd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func processGraphNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	if os.Getenv("CI") != "" {
		t.Fatal("node not on PATH in CI — process graph JS tests did not run")
	}
	t.Skip("node not on PATH — skipping process graph JS tests")
	return ""
}

// Syntax-check each shipped module as an ES module, exactly matching the
// dashboard's native-module loading mode. --input-type applies to stdin, so the
// source is streamed rather than passed as a CommonJS-classified .js path.
func TestProcessGraph_JSSyntax(t *testing.T) {
	node := processGraphNode(t)
	for _, file := range []string{
		"dashboard/js/process-layout.js",
		"dashboard/js/process-graph.js",
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

func TestProcessGraph_LayoutJS(t *testing.T) {
	node := processGraphNode(t)
	file := filepath.Join("jstest", "process-layout.test.mjs")
	out, err := exec.Command(node, "--test", file).CombinedOutput()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("process layout JS unit tests failed: %v\n%s", err, out)
		}
		if os.Getenv("CI") != "" {
			t.Fatalf("node not runnable in CI: %v", err)
		}
		t.Skipf("unable to run node: %v", err)
	}
	t.Logf("node --test %s:\n%s", file, out)
}
