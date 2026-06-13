package agentd

import (
	"slices"
	"testing"
)

// TestSessionNewArgs_EffortOmittedWhenUnset is the acceptance check for
// the spawn path's forked `tclaude session new`: with no effort chosen,
// the argv must carry no --effort flag, so claude uses its own default.
func TestSessionNewArgs_EffortOmittedWhenUnset(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "", "")
	if slices.Contains(args, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %v", args)
	}
}

// TestSessionNewArgs_EffortIncludedWhenSet verifies an explicit level is
// passed through as `--effort <level>` to the forked session.
func TestSessionNewArgs_EffortIncludedWhenSet(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "high", "", "", "")
	i := slices.Index(args, "--effort")
	if i < 0 || i+1 >= len(args) || args[i+1] != "high" {
		t.Fatalf("set effort must append `--effort high`, got %v", args)
	}
}

// TestSessionNewArgs_Harness covers the --harness flag: omitted for the
// default (""/claude) so an untagged spawn keeps the exact pre-JOH-160
// argv, and appended as `--harness codex` for a non-default harness.
func TestSessionNewArgs_Harness(t *testing.T) {
	for _, h := range []string{"", "claude"} {
		if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", h, ""), "--harness") {
			t.Fatalf("harness %q must omit --harness (default), got flag", h)
		}
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "")
	i := slices.Index(args, "--harness")
	if i < 0 || i+1 >= len(args) || args[i+1] != "codex" {
		t.Fatalf("codex harness must append `--harness codex`, got %v", args)
	}
	rargs := sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "")
	if ri := slices.Index(rargs, "--harness"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "codex" {
		t.Fatalf("resume must append `--harness codex`, got %v", rargs)
	}
}

// TestSessionNewArgs_Sandbox covers the --sandbox flag: omitted when no
// mode was resolved (""), and appended as `--sandbox <mode>` for a Codex
// spawn/resume. The mode is resolved + cwd-guarded at the spawn boundary,
// so by the time it reaches the argv builder it is a validated enum.
func TestSessionNewArgs_Sandbox(t *testing.T) {
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", ""), "--sandbox") {
		t.Fatalf("unset sandbox must omit --sandbox")
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write")
	i := slices.Index(args, "--sandbox")
	if i < 0 || i+1 >= len(args) || args[i+1] != "workspace-write" {
		t.Fatalf("set sandbox must append `--sandbox workspace-write`, got %v", args)
	}
	rargs := sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "workspace-write")
	if ri := slices.Index(rargs, "--sandbox"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "workspace-write" {
		t.Fatalf("resume must append `--sandbox workspace-write`, got %v", rargs)
	}
}
