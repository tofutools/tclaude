package agentd

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

func dashboardTestNode(t *testing.T, suite string) string {
	t.Helper()
	ci := os.Getenv("CI") != ""
	nodeCacheKey := os.Getenv("TCLAUDE_NODE_TEST_CACHE_KEY")
	node, err := exec.LookPath("node")
	if err != nil {
		if ci {
			t.Fatalf("node not on PATH in CI — %s JS tests did not run", suite)
		}
		t.Skipf("node not on PATH — skipping %s JS tests", suite)
	}
	if ci && nodeCacheKey == "" {
		t.Fatal("TCLAUDE_NODE_TEST_CACHE_KEY is empty in CI — the external Node runtime " +
			"must participate in Go's test-cache key")
	}
	return node
}

// Syntax-check each shipped module as an ES module, exactly matching the
// dashboard's native-module loading mode. --input-type applies to stdin, so the
// source is streamed rather than passed as a CommonJS-classified .js path.
func TestProcessGraph_JSSyntax(t *testing.T) {
	node := dashboardTestNode(t, "process graph")
	for _, file := range []string{
		"dashboard/js/process-layout.js",
		"dashboard/js/process-selection.js",
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
