package agentd

import (
	"slices"
	"testing"
)

// TestSessionNewArgs_ModelOmittedWhenUnset is the acceptance check for
// the spawn path's forked `tclaude session new`: with no model chosen,
// the argv must carry no --model flag, so claude uses its own default.
func TestSessionNewArgs_ModelOmittedWhenUnset(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "", "")
	if slices.Contains(args, "--model") {
		t.Fatalf("unset model must omit --model, got %v", args)
	}
}

// TestSessionNewArgs_ModelIncludedWhenSet verifies an explicit alias is
// passed through as `--model <alias>` to the forked session.
func TestSessionNewArgs_ModelIncludedWhenSet(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "", "opus[1m]")
	i := slices.Index(args, "--model")
	if i < 0 || i+1 >= len(args) || args[i+1] != "opus[1m]" {
		t.Fatalf("set model must append `--model opus[1m]`, got %v", args)
	}
}
