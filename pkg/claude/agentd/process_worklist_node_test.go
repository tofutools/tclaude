package agentd

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// Mirrors process_graph_node_test.go: the worklist sub-view has no build
// step, so its pure view/filter/format core is exercised under plain Node
// (skipped locally when node is absent, fatal in CI).

// Syntax-check each shipped worklist module as an ES module, matching the
// dashboard's native-module loading mode.
func TestProcessWorklist_JSSyntax(t *testing.T) {
	node := dashboardTestNode(t, "process worklist")
	for _, file := range []string{
		"dashboard/js/process-worklist-core.js",
		"dashboard/js/processes-actions.js",
		"dashboard/js/processes-island.js",
		"dashboard/js/processes-state.js",
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
