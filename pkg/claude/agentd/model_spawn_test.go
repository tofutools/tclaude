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

// TestSessionResumeArgs_ModelOmittedWhenUnset mirrors the spawn-path
// check for the resume fork: no inherited model ⇒ no --model flag, so
// claude resolves its own default (the fail-open inheritance contract).
func TestSessionResumeArgs_ModelOmittedWhenUnset(t *testing.T) {
	args := sessionResumeArgs("conv-1", "/tmp/x", "", "")
	if slices.Contains(args, "--model") || slices.Contains(args, "--effort") {
		t.Fatalf("unset model/effort must omit the flags, got %v", args)
	}
}

// TestSessionResumeArgs_ModelIncludedWhenSet verifies the inherited
// model + effort ride the forked `tclaude session new -r` argv — the
// seam clone-copy and agent-resume reach liveSpawnResume through.
func TestSessionResumeArgs_ModelIncludedWhenSet(t *testing.T) {
	args := sessionResumeArgs("conv-1", "/tmp/x", "high", "claude-fable-5[1m]")
	if i := slices.Index(args, "--model"); i < 0 || i+1 >= len(args) || args[i+1] != "claude-fable-5[1m]" {
		t.Fatalf("inherited model must append `--model claude-fable-5[1m]`, got %v", args)
	}
	if i := slices.Index(args, "--effort"); i < 0 || i+1 >= len(args) || args[i+1] != "high" {
		t.Fatalf("inherited effort must append `--effort high`, got %v", args)
	}
	if i := slices.Index(args, "-r"); i < 0 || i+1 >= len(args) || args[i+1] != "conv-1" {
		t.Fatalf("resume args must keep `-r <conv>`, got %v", args)
	}
}
