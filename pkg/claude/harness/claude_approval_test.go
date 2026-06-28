package harness

import (
	"strings"
	"testing"
)

// TestClaudeApproval_Catalog pins the catalog the spawn dialog / profile editor
// drive their Claude "Permission mode" selector off: inherit + the six
// --permission-mode values, the inherit default, and inherit→"" normalization.
func TestClaudeApproval_Catalog(t *testing.T) {
	c := claudeApproval{}

	want := []string{"inherit", "plan", "default", "acceptEdits", "auto", "dontAsk", "bypassPermissions"}
	if got := c.Modes(); !equalStrings(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
	if got := c.DefaultPolicy(); got != claudePermInherit {
		t.Fatalf("DefaultPolicy() = %q, want %q", got, claudePermInherit)
	}

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"inherit", "", false},
		{" plan ", "plan", false},
		{"acceptEdits", "acceptEdits", false},
		{"bypassPermissions", "bypassPermissions", false},
		{"never", "", true},       // a Codex policy is not a Claude mode
		{"acceptedits", "", true}, // case-sensitive: Claude's token is camelCase
	}
	for _, tc := range cases {
		got, err := c.ValidatePolicy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ValidatePolicy(%q) = (%q, nil), want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ValidatePolicy(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}

	for _, m := range c.Modes() {
		if c.ModeHelp(m) == "" {
			t.Fatalf("ModeHelp(%q) is empty", m)
		}
	}
	if strings.Contains(c.ModeHelp("inherit"), "⚠") {
		t.Fatalf("inherit (the default) must carry no ⚠ caveat: %q", c.ModeHelp("inherit"))
	}
	if !strings.Contains(c.ModeHelp("bypassPermissions"), "⚠") {
		t.Fatalf("bypassPermissions must flag a ⚠ caveat: %q", c.ModeHelp("bypassPermissions"))
	}
}

// TestClaudeApproval_HarnessResolution pins the harness-level wiring: Claude
// supports approval (its permission modes) but NOT auto-review (no guardian),
// and inherit/blank resolves to "" (omit --permission-mode).
func TestClaudeApproval_HarnessResolution(t *testing.T) {
	h, err := Resolve(DefaultName)
	if err != nil {
		t.Fatalf("Resolve(claude): %v", err)
	}
	if !h.SupportsApproval() {
		t.Fatal("claude must SupportsApproval (permission modes)")
	}
	if h.SupportsAutoReview() {
		t.Fatal("claude must NOT SupportsAutoReview — it has no guardian subagent")
	}

	// Daemon path: blank resolves to inherit, which normalizes to "" (omit).
	if got, err := ResolveApprovalPolicy(h, ""); err != nil || got != "" {
		t.Fatalf("ResolveApprovalPolicy(claude, \"\") = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, err := ResolveApprovalPolicy(h, "plan"); err != nil || got != "plan" {
		t.Fatalf("ResolveApprovalPolicy(claude, plan) = (%q, %v), want (plan, nil)", got, err)
	}
	// --auto-review must still be rejected for claude (no reviewer).
	if _, err := ResolveAutoReview(h, true); err == nil {
		t.Fatal("ResolveAutoReview(claude, true) must error — claude has no approvals reviewer")
	}
}

// TestClaudeApproval_CodexNotSurfaced guards the "Claude only" scope: Codex's
// approval is real (DefaultPolicy still drives the daemon default) but exposes
// no dialog modes, so the dashboard keeps the Codex approval row hidden.
func TestClaudeApproval_CodexNotSurfaced(t *testing.T) {
	h, err := Resolve(CodexName)
	if err != nil {
		t.Fatalf("Resolve(codex): %v", err)
	}
	if !h.SupportsApproval() {
		t.Fatal("codex still supports approval (the daemon default + CLI)")
	}
	if modes := h.Approval.Modes(); len(modes) != 0 {
		t.Fatalf("codex must surface no dialog approval modes (Claude-only scope), got %v", modes)
	}
	if !h.SupportsAutoReview() {
		t.Fatal("codex must SupportsAutoReview — it has the guardian subagent")
	}
}

// TestClaudeSpawner_Approval is the acceptance check for the spawn surface: an
// explicit permission mode emits `--permission-mode <mode>` (Claude's flag, not
// Codex's --ask-for-approval); inherit / unset emits nothing.
func TestClaudeSpawner_Approval(t *testing.T) {
	spawn := func(policy string) string {
		return claudeSpawner{}.BuildCommand(SpawnSpec{ApprovalPolicy: policy})
	}

	if got := spawn(""); strings.Contains(got, "--permission-mode") {
		t.Fatalf("unset approval must omit --permission-mode, got %q", got)
	}
	if got := spawn("plan"); !strings.Contains(got, "--permission-mode plan") {
		t.Fatalf("plan must emit --permission-mode plan, got %q", got)
	}
	// Claude never gets Codex's --ask-for-approval.
	if got := spawn("bypassPermissions"); strings.Contains(got, "--ask-for-approval") {
		t.Fatalf("claude must use --permission-mode, never --ask-for-approval, got %q", got)
	}
}
