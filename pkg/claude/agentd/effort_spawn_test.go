package agentd

import (
	"slices"
	"testing"
)

// TestSessionNewArgs_EffortOmittedWhenUnset is the acceptance check for
// the spawn path's forked `tclaude session new`: with no effort chosen,
// the argv must carry no --effort flag, so claude uses its own default.
func TestSessionNewArgs_EffortOmittedWhenUnset(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "", "")
	if slices.Contains(args, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %v", args)
	}
}

// TestSessionNewArgs_EffortIncludedWhenSet verifies an explicit level is
// passed through as `--effort <level>` to the forked session.
func TestSessionNewArgs_EffortIncludedWhenSet(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "high", "")
	i := slices.Index(args, "--effort")
	if i < 0 || i+1 >= len(args) || args[i+1] != "high" {
		t.Fatalf("set effort must append `--effort high`, got %v", args)
	}
}
