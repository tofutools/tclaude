package agentd

import (
	"slices"
	"testing"
)

// TestSessionNewArgs_EffortOmittedWhenUnset is the acceptance check for
// the spawn path's forked `tclaude session new`: with no effort chosen,
// the argv must carry no --effort flag, so claude uses its own default.
func TestSessionNewArgs_EffortOmittedWhenUnset(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "", "", "", false, false)
	if slices.Contains(args, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %v", args)
	}
}

// TestSessionNewArgs_CodexGetsInitialPromptSeed checks the JOH-205 first-turn
// seed: a daemon-spawned Codex carries `--initial-prompt <seed>` so it takes a
// turn (materialising its conv-id) without a human, while Claude Code — which
// reports its conv-id at launch — never gets the seed.
func TestSessionNewArgs_CodexGetsInitialPromptSeed(t *testing.T) {
	codex := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false, false)
	i := slices.Index(codex, "--initial-prompt")
	if i < 0 || i+1 >= len(codex) || codex[i+1] != codexSpawnSeedPrompt {
		t.Fatalf("codex spawn must carry --initial-prompt %q, got %v", codexSpawnSeedPrompt, codex)
	}

	// Default harness (Claude Code) reports its conv-id at launch — no seed.
	if cc := sessionNewArgs("lbl", "/tmp/x", "", "", "", "", "", false, false); slices.Contains(cc, "--initial-prompt") {
		t.Fatalf("Claude Code must NOT get an initial-prompt seed, got %v", cc)
	}
}

// TestSessionNewArgs_EffortIncludedWhenSet verifies an explicit level is
// passed through as `--effort <level>` to the forked session.
func TestSessionNewArgs_EffortIncludedWhenSet(t *testing.T) {
	args := sessionNewArgs("lbl", "/tmp/x", "high", "", "", "", "", false, false)
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
		if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", h, "", "", false, false), "--harness") {
			t.Fatalf("harness %q must omit --harness (default), got flag", h)
		}
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "", "", false, false)
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
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "", "", false, false), "--sandbox") {
		t.Fatalf("unset sandbox must omit --sandbox")
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "", false, false)
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
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "", "", false, false), "--ask-for-approval") {
		t.Fatalf("unset approval must omit --ask-for-approval")
	}
	args := sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false, false)
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
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false, false), "--auto-review") {
		t.Fatalf("autoReview=false must omit --auto-review")
	}
	if !slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", true, false), "--auto-review") {
		t.Fatalf("autoReview=true must append --auto-review")
	}
	if !slices.Contains(sessionResumeArgs("conv-1", "/tmp/x", "", "", "codex", "workspace-write", "never", true), "--auto-review") {
		t.Fatalf("resume autoReview=true must append --auto-review")
	}
}

// TestSessionNewArgs_TrustDir covers the --trust-dir flag (JOH-205 inc4): a
// bare boolean flag appended only when the spawn opted into pre-trusting its
// launch dir for Codex (true), omitted otherwise. The opt-in is gated at the
// spawn boundary (harness.ResolveTrustDir) before it reaches the argv builder;
// relaunch paths (reincarnate/clone) always pass false.
func TestSessionNewArgs_TrustDir(t *testing.T) {
	if slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false, false), "--trust-dir") {
		t.Fatalf("trustDir=false must omit --trust-dir")
	}
	if !slices.Contains(sessionNewArgs("lbl", "/tmp/x", "", "", "codex", "workspace-write", "never", false, true), "--trust-dir") {
		t.Fatalf("trustDir=true must append --trust-dir")
	}
}
