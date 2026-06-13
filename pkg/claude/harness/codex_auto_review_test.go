package harness

import (
	"strings"
	"testing"
)

// TestResolveAutoReview covers the auto-review opt-in gate: it passes through
// for a harness with an approvals subsystem (Codex) on both true and false,
// rejects an opt-in for a harness with no guardian (Claude Code), and leaves
// false alone for that harness (the default is off everywhere, so a non-opt-in
// on a non-guardian harness is fine — only an explicit true is an error).
func TestResolveAutoReview(t *testing.T) {
	codex := MustGet(CodexName)
	claude := MustGet(DefaultName)

	if got, err := ResolveAutoReview(codex, true); err != nil || got != true {
		t.Fatalf("ResolveAutoReview(codex, true) = %v,%v; want true,nil", got, err)
	}
	if got, err := ResolveAutoReview(codex, false); err != nil || got != false {
		t.Fatalf("ResolveAutoReview(codex, false) = %v,%v; want false,nil", got, err)
	}
	if got, err := ResolveAutoReview(claude, false); err != nil || got != false {
		t.Fatalf("ResolveAutoReview(claude, false) = %v,%v; want false,nil (no opt-in, no error)", got, err)
	}
	if _, err := ResolveAutoReview(claude, true); err == nil {
		t.Fatalf("ResolveAutoReview(claude, true) must error — Claude Code has no approvals reviewer")
	}
}

// TestCodexSpawner_AutoReviewFlag verifies the emitted Codex args carry the
// `-c approvals_reviewer="auto_review"` guardian override when AutoReview is
// set (on both fresh + resume, coexisting with --ask-for-approval) and omit it
// when unset — the JOH-200 part 2 acceptance at the literal arg surface.
func TestCodexSpawner_AutoReviewFlag(t *testing.T) {
	// The whole `key="value"` is one shell-quoted arg; since the value
	// carries double quotes, ShellQuoteArg wraps it in single quotes (same
	// as the effort `-c model_reasoning_effort="…"` override).
	const wantOverride = `-c 'approvals_reviewer="auto_review"'`

	// Unset → no override.
	if got := (codexSpawner{}).BuildCommand(SpawnSpec{}); strings.Contains(got, "approvals_reviewer") {
		t.Fatalf("unset AutoReview must omit the approvals_reviewer override, got %q", got)
	}
	// Fresh spawn with auto-review on.
	got := (codexSpawner{}).BuildCommand(SpawnSpec{AutoReview: true})
	if !strings.Contains(got, wantOverride) {
		t.Fatalf("AutoReview spawn must emit %s, got %q", wantOverride, got)
	}
	// Resume with auto-review on, composing with an explicit approval policy
	// (the two axes are orthogonal and must coexist on one invocation).
	gotR := (codexSpawner{}).BuildCommand(SpawnSpec{
		ResumeID:       "abc-123",
		ApprovalPolicy: ApprovalOnRequest,
		AutoReview:     true,
	})
	if !strings.Contains(gotR, "resume abc-123") ||
		!strings.Contains(gotR, "--ask-for-approval on-request") ||
		!strings.Contains(gotR, wantOverride) {
		t.Fatalf("resume must carry `resume abc-123` + `--ask-for-approval on-request` + %s, got %q", wantOverride, gotR)
	}
}
