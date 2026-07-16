package harness

import (
	"slices"
	"strings"
	"testing"
)

// TestCodexApproval_DefaultPolicy pins the non-escalating default — the
// property the whole unattended-deadlock fix rests on (JOH-200). If this ever
// drifts to an escalating policy (on-request / on-failure / untrusted), a
// detached Codex pane would block on a prompt no human can answer.
func TestCodexApproval_DefaultPolicy(t *testing.T) {
	if got := (codexApproval{}).DefaultPolicy(); got != ApprovalNever {
		t.Fatalf("codex approval default = %q, want %q (non-escalating, so an unattended pane can't deadlock)", got, ApprovalNever)
	}
}

func TestCodexApproval_ValidatePolicy(t *testing.T) {
	ca := codexApproval{}
	for _, ok := range []string{"", ApprovalUntrusted, ApprovalOnFailure, ApprovalOnRequest, ApprovalNever} {
		if got, err := ca.ValidatePolicy(ok); err != nil || got != ok {
			t.Fatalf("ValidatePolicy(%q) = %q,%v; want %q,nil", ok, got, err, ok)
		}
	}
	if got, err := ca.ValidatePolicy("  never  "); err != nil || got != ApprovalNever {
		t.Fatalf("ValidatePolicy trims whitespace: got %q,%v; want %q,nil", got, err, ApprovalNever)
	}
	if _, err := ca.ValidatePolicy("yolo"); err == nil {
		t.Fatalf("ValidatePolicy(yolo) must error")
	}
}

func TestCodexApproval_ModesShareValidationCatalog(t *testing.T) {
	ca := codexApproval{}
	want := []string{ApprovalNever, ApprovalUntrusted, ApprovalOnFailure, ApprovalOnRequest}
	if got := ca.Modes(); !slices.Equal(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
	for _, mode := range ca.Modes() {
		if got, err := ca.ValidatePolicy(mode); err != nil || got != mode {
			t.Fatalf("ValidatePolicy(%q) = (%q, %v)", mode, got, err)
		}
		if strings.TrimSpace(ca.ModeHelp(mode)) == "" {
			t.Fatalf("ModeHelp(%q) is empty", mode)
		}
	}
	got := ca.Modes()
	got[0] = "mutated"
	if ca.Modes()[0] != ApprovalNever {
		t.Fatal("Modes must return a fresh slice")
	}
	if ca.ModeHelp("unknown") != "" {
		t.Fatal("unknown mode must have no help")
	}
}

// TestResolveApprovalPolicy covers the daemon spawn-boundary entry point: a
// Codex agent gets the non-escalating default (never) when unset; an explicit
// policy is validated; and a harness with no launch approval flag (Claude
// Code) defaults to "" but rejects an explicit policy.
func TestResolveApprovalPolicy(t *testing.T) {
	codex := MustGet(CodexName)
	claude := MustGet(DefaultName)

	if got, err := ResolveApprovalPolicy(codex, ""); err != nil || got != ApprovalNever {
		t.Fatalf("ResolveApprovalPolicy(codex, \"\") = %q,%v; want %q,nil", got, err, ApprovalNever)
	}
	if got, err := ResolveApprovalPolicy(codex, ApprovalOnRequest); err != nil || got != ApprovalOnRequest {
		t.Fatalf("ResolveApprovalPolicy(codex, on-request) = %q,%v; want %q,nil", got, err, ApprovalOnRequest)
	}
	if _, err := ResolveApprovalPolicy(codex, "nope"); err == nil {
		t.Fatalf("ResolveApprovalPolicy(codex, nope) must error")
	}
	// Claude: unset → the inherit default, carried as the first-class "inherit"
	// (it emits no --permission-mode; its permission posture is settings.json-driven).
	if got, err := ResolveApprovalPolicy(claude, ""); err != nil || got != "inherit" {
		t.Fatalf("ResolveApprovalPolicy(claude, \"\") = %q,%v; want \"inherit\",nil", got, err)
	}
	if _, err := ResolveApprovalPolicy(claude, ApprovalNever); err == nil {
		t.Fatalf("ResolveApprovalPolicy(claude, never) must error — claude has no --ask-for-approval")
	}
}

// TestValidateApprovalPolicy is the no-default variant the direct `session
// new` path uses: the human is the trust root (they can attach to answer
// prompts), so an unset policy stays "" (no flag) rather than being forced to
// the daemon's non-escalating default.
func TestValidateApprovalPolicy(t *testing.T) {
	codex := MustGet(CodexName)
	claude := MustGet(DefaultName)

	if got, err := ValidateApprovalPolicy(codex, ""); err != nil || got != "" {
		t.Fatalf("ValidateApprovalPolicy(codex, \"\") = %q,%v; want \"\",nil (must not default)", got, err)
	}
	if got, err := ValidateApprovalPolicy(codex, ApprovalNever); err != nil || got != ApprovalNever {
		t.Fatalf("ValidateApprovalPolicy(codex, never) = %q,%v; want %q,nil", got, err, ApprovalNever)
	}
	if _, err := ValidateApprovalPolicy(codex, "nope"); err == nil {
		t.Fatalf("ValidateApprovalPolicy(codex, nope) must error")
	}
	if got, err := ValidateApprovalPolicy(claude, ""); err != nil || got != "" {
		t.Fatalf("ValidateApprovalPolicy(claude, \"\") = %q,%v; want \"\",nil", got, err)
	}
	if _, err := ValidateApprovalPolicy(claude, ApprovalNever); err == nil {
		t.Fatalf("ValidateApprovalPolicy(claude, never) must error")
	}
}

// TestCodexSpawner_ApprovalFlag verifies the emitted Codex args carry
// `--ask-for-approval <policy>` when a policy is set (on both fresh + resume)
// and omit it when unset — the JOH-200 acceptance at the literal arg surface.
func TestCodexSpawner_ApprovalFlag(t *testing.T) {
	// Unset → no flag.
	if got := (codexSpawner{}).BuildCommand(SpawnSpec{}); strings.Contains(got, "--ask-for-approval") {
		t.Fatalf("unset approval must omit --ask-for-approval, got %q", got)
	}
	// Fresh spawn with a policy.
	got := (codexSpawner{}).BuildCommand(SpawnSpec{ApprovalPolicy: ApprovalNever})
	if !strings.Contains(got, "--ask-for-approval never") {
		t.Fatalf("fresh spawn must emit `--ask-for-approval never`, got %q", got)
	}
	// Resume with a policy (shared global flag), coexisting with --sandbox.
	gotR := (codexSpawner{}).BuildCommand(SpawnSpec{ResumeID: "abc-123", SandboxMode: SandboxWorkspaceWrite, ApprovalPolicy: ApprovalNever})
	if !strings.Contains(gotR, "resume abc-123") || !strings.Contains(gotR, "--ask-for-approval never") || !strings.Contains(gotR, "--sandbox workspace-write") {
		t.Fatalf("resume must carry the resume subcommand + `--ask-for-approval never` + `--sandbox workspace-write`, got %q", gotR)
	}
}
