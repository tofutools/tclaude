package agentd

import (
	"slices"
	"testing"
)

// TestSessionNewArgs_EffortOmittedWhenUnset is the acceptance check for
// the spawn path's forked `tclaude session new`: with no effort chosen,
// the argv must carry no --effort flag, so claude uses its own default.
func TestSessionNewArgs_EffortOmittedWhenUnset(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "", "", "", false)
	if slices.Contains(args, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %v", args)
	}
}

// TestSessionNewArgs_EffortIncludedWhenSet verifies an explicit level is
// passed through as `--effort <level>` to the forked session.
func TestSessionNewArgs_EffortIncludedWhenSet(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "high", "", "", "", "", false)
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
		if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", h, "", "", false), "--harness") {
			t.Fatalf("harness %q must omit --harness (default), got flag", h)
		}
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "", "", false)
	i := slices.Index(args, "--harness")
	if i < 0 || i+1 >= len(args) || args[i+1] != "codex" {
		t.Fatalf("codex harness must append `--harness codex`, got %v", args)
	}
	rargs := sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "", "", false)
	if ri := slices.Index(rargs, "--harness"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "codex" {
		t.Fatalf("resume must append `--harness codex`, got %v", rargs)
	}
}

// TestSessionNewArgs_Sandbox covers the --sandbox flag: omitted when no
// mode was resolved (""), and appended as `--sandbox <mode>` for a Codex
// spawn/resume. The mode is resolved + cwd-guarded at the spawn boundary,
// so by the time it reaches the argv builder it is a validated enum.
func TestSessionNewArgs_Sandbox(t *testing.T) {
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "", "", false), "--sandbox") {
		t.Fatalf("unset sandbox must omit --sandbox")
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "", false)
	i := slices.Index(args, "--sandbox")
	if i < 0 || i+1 >= len(args) || args[i+1] != "workspace-write" {
		t.Fatalf("set sandbox must append `--sandbox workspace-write`, got %v", args)
	}
	rargs := sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "workspace-write", "", false)
	if ri := slices.Index(rargs, "--sandbox"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "workspace-write" {
		t.Fatalf("resume must append `--sandbox workspace-write`, got %v", rargs)
	}
}

// TestSessionNewArgs_Approval covers the --ask-for-approval flag: omitted
// when no policy was resolved (""), and appended as `--ask-for-approval
// <policy>` for a Codex spawn/resume. The policy is resolved at the spawn
// boundary (harness.ResolveApprovalPolicy → "never" for an unattended Codex
// pane), so by the time it reaches the argv builder it is a validated enum
// (JOH-200).
func TestSessionNewArgs_Approval(t *testing.T) {
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "", "", false), "--ask-for-approval") {
		t.Fatalf("unset approval must omit --ask-for-approval")
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false)
	i := slices.Index(args, "--ask-for-approval")
	if i < 0 || i+1 >= len(args) || args[i+1] != "never" {
		t.Fatalf("set approval must append `--ask-for-approval never`, got %v", args)
	}
	rargs := sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "workspace-write", "never", false)
	if ri := slices.Index(rargs, "--ask-for-approval"); ri < 0 || ri+1 >= len(rargs) || rargs[ri+1] != "never" {
		t.Fatalf("resume must append `--ask-for-approval never`, got %v", rargs)
	}
}

// TestSessionNewArgs_AutoReview covers the --auto-review flag: a bare boolean
// flag appended only when the spawn opted in (true), omitted otherwise. The
// opt-in is gated at the spawn boundary (harness.ResolveAutoReview) before it
// reaches the argv builder; relaunch paths always pass false (JOH-200 part 2).
func TestSessionNewArgs_AutoReview(t *testing.T) {
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false), "--auto-review") {
		t.Fatalf("autoReview=false must omit --auto-review")
	}
	if !slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", true), "--auto-review") {
		t.Fatalf("autoReview=true must append --auto-review")
	}
	if !slices.Contains(sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "workspace-write", "never", true), "--auto-review") {
		t.Fatalf("resume autoReview=true must append --auto-review")
	}
}
