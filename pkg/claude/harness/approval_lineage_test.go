package harness

import (
	"strings"
	"testing"
)

func TestApprovalLineageAllowedMatrix(t *testing.T) {
	tests := []struct {
		name                        string
		parentHarness, parentPolicy string
		parentAutoReview            bool
		childHarness, childPolicy   string
		childAutoReview             bool
		want                        bool
	}{
		// --- TCL-576 required allows: cross-harness, both directions ---
		{"codex never to claude auto", CodexName, ApprovalNever, false, DefaultName, claudePermAuto, false, true},
		{"codex never plus idle guardian to claude auto", CodexName, ApprovalNever, true, DefaultName, claudePermAuto, false, true},
		{"codex on-request to claude auto", CodexName, ApprovalOnRequest, false, DefaultName, claudePermAuto, false, true},
		{"codex on-failure to claude auto", CodexName, ApprovalOnFailure, false, DefaultName, claudePermAuto, false, true},
		{"codex guardian to claude accept edits", CodexName, ApprovalOnRequest, true, DefaultName, claudePermAccept, false, true},
		{"codex never to claude accept edits", CodexName, ApprovalNever, false, DefaultName, claudePermAccept, false, true},
		{"codex baseline to claude default", CodexName, ApprovalOnRequest, false, DefaultName, claudePermDefault, false, true},
		{"claude auto to codex never", DefaultName, claudePermAuto, false, CodexName, ApprovalNever, false, true},
		{"claude inherit to codex never", DefaultName, claudePermInherit, false, CodexName, ApprovalNever, false, true},
		// inherit is classified as the broadest non-bypass posture, which is
		// exactly Codex on-request + Auto-review. Pin both directions of that
		// equality so the collision is a decision, not an accident.
		{"claude inherit to codex guardian", DefaultName, claudePermInherit, false, CodexName, ApprovalOnRequest, true, true},
		{"codex guardian to claude inherit", CodexName, ApprovalOnRequest, true, DefaultName, claudePermInherit, false, true},

		// --- TCL-576 required allows: an inherit parent is not spawn-crippled ---
		{"claude inherit to claude inherit", DefaultName, claudePermInherit, false, DefaultName, claudePermInherit, false, true},
		{"claude inherit to claude auto", DefaultName, claudePermInherit, false, DefaultName, claudePermAuto, false, true},
		{"claude inherit to claude plan", DefaultName, claudePermInherit, false, DefaultName, claudePermPlan, false, true},
		{"claude accept edits to identical shape", DefaultName, claudePermAccept, false, DefaultName, claudePermAccept, false, true},
		{"claude auto to identical shape", DefaultName, claudePermAuto, false, DefaultName, claudePermAuto, false, true},
		{"claude auto to claude accept edits", DefaultName, claudePermAuto, false, DefaultName, claudePermAccept, false, true},
		{"claude dontAsk to claude plan", DefaultName, claudePermDontAsk, false, DefaultName, claudePermPlan, false, true},
		{"claude dontAsk to codex untrusted", DefaultName, claudePermDontAsk, false, CodexName, ApprovalUntrusted, false, true},

		// acceptEdits auto-approves EDITS only; every other command still prompts
		// a human. It must not be able to mint a child that runs arbitrary
		// commands unattended, in either harness.
		{"accept edits cannot mint codex never", DefaultName, claudePermAccept, false, CodexName, ApprovalNever, false, false},
		{"accept edits cannot mint codex on-request", DefaultName, claudePermAccept, false, CodexName, ApprovalOnRequest, false, false},
		{"accept edits cannot mint claude auto", DefaultName, claudePermAccept, false, DefaultName, claudePermAuto, false, false},
		{"claude dontAsk cannot mint accept edits", DefaultName, claudePermDontAsk, false, DefaultName, claudePermAccept, false, false},

		// --- Bypass stays gated: only an equally bypassed parent, or a human ---
		{"codex never cannot mint claude bypass", CodexName, ApprovalNever, false, DefaultName, claudePermBypass, false, false},
		{"claude inherit cannot mint claude bypass", DefaultName, claudePermInherit, false, DefaultName, claudePermBypass, false, false},
		{"claude auto cannot mint claude bypass", DefaultName, claudePermAuto, false, DefaultName, claudePermBypass, false, false},
		{"claude bypass to any posture", DefaultName, claudePermBypass, false, DefaultName, claudePermInherit, false, true},
		{"claude bypass to codex guardian", DefaultName, claudePermBypass, false, CodexName, ApprovalOnRequest, true, true},

		// --- An unresolvable inherit CHILD fails closed under a narrower parent ---
		{"codex never cannot mint claude inherit", CodexName, ApprovalNever, false, DefaultName, claudePermInherit, false, false},
		{"claude auto cannot mint claude inherit", DefaultName, claudePermAuto, false, DefaultName, claudePermInherit, false, false},
		{"claude plan cannot mint claude inherit", DefaultName, claudePermPlan, false, DefaultName, claudePermInherit, false, false},

		// --- Genuinely broader capability is still denied, both directions ---
		{"claude accept edits cannot enable codex guardian", DefaultName, claudePermAccept, false, CodexName, ApprovalOnRequest, true, false},
		{"claude auto cannot enable codex guardian", DefaultName, claudePermAuto, false, CodexName, ApprovalUntrusted, true, false},
		{"claude plan cannot delegate in-sandbox execution", DefaultName, claudePermPlan, false, DefaultName, claudePermAuto, false, false},
		{"claude default cannot delegate in-sandbox execution", DefaultName, claudePermDefault, false, CodexName, ApprovalNever, false, false},
		{"codex untrusted cannot delegate claude auto", CodexName, ApprovalUntrusted, false, DefaultName, claudePermAuto, false, false},

		// --- Same-harness Codex lineage is unchanged ---
		{"codex baseline to codex baseline", CodexName, ApprovalOnRequest, false, CodexName, ApprovalNever, false, true},
		{"codex untrusted cannot delegate sandbox-auto never", CodexName, ApprovalUntrusted, false, CodexName, ApprovalNever, false, false},
		{"codex untrusted to untrusted", CodexName, ApprovalUntrusted, false, CodexName, ApprovalUntrusted, false, true},
		{"codex baseline cannot enable reviewer", CodexName, ApprovalNever, false, CodexName, ApprovalOnRequest, true, false},
		{"codex untrusted reviewer cannot delegate sandbox-auto never", CodexName, ApprovalUntrusted, true, CodexName, ApprovalNever, false, false},
		{"codex untrusted reviewer to same", CodexName, ApprovalUntrusted, true, CodexName, ApprovalUntrusted, true, true},
		{"codex reviewer to codex reviewer", CodexName, ApprovalOnRequest, true, CodexName, ApprovalUntrusted, true, true},

		// --- Malformed / unclassifiable postures fail closed ---
		{"legacy blank codex parent fails closed", CodexName, "", false, CodexName, ApprovalNever, false, false},
		{"legacy blank claude parent fails closed", DefaultName, "", false, DefaultName, claudePermAuto, false, false},
		{"legacy blank claude child fails closed", DefaultName, claudePermBypass, false, DefaultName, "", false, false},
		{"claude auto-review is malformed on the parent", DefaultName, claudePermDefault, true, DefaultName, claudePermDefault, false, false},
		{"claude auto-review is malformed on the child", DefaultName, claudePermBypass, false, DefaultName, claudePermAuto, true, false},
		{"unknown harness fails closed", "gemini", "whatever", false, DefaultName, claudePermPlan, false, false},
		{"unknown child harness fails closed", DefaultName, claudePermBypass, false, "gemini", "whatever", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ApprovalLineageAllowed(tt.parentHarness, tt.parentPolicy, tt.parentAutoReview,
				tt.childHarness, tt.childPolicy, tt.childAutoReview); got != tt.want {
				t.Fatalf("ApprovalLineageAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

// An empty harness name is the historic spelling of "Claude", on both sides.
func TestApprovalLineageBlankHarnessIsClaude(t *testing.T) {
	if !ApprovalLineageAllowed("", claudePermInherit, false, "", claudePermAuto, false) {
		t.Fatal("blank harness names must classify as Claude on both sides")
	}
	// The blank spelling must not become an escape hatch: it is still gated.
	if ApprovalLineageAllowed("", claudePermAuto, false, "", claudePermBypass, false) {
		t.Fatal("a blank-harness parent must not be able to mint bypass")
	}
}

// A denial must name a way out that ACTUALLY works — a hint the caller can
// retry into a second 403 is worse than no hint.
func TestApprovalLineageDenialHint(t *testing.T) {
	// A parent holding full in-sandbox execution can delegate `auto`.
	inherit := ApprovalLineageDenialHint(CodexName, ApprovalNever, false, DefaultName, claudePermInherit)
	if !strings.Contains(inherit, claudePermAuto) {
		t.Fatalf("inherit hint must point at %q, got %q", claudePermAuto, inherit)
	}

	// An acceptEdits parent may NOT delegate `auto`, so the hint must name the
	// widest mode it can actually delegate instead.
	accept := ApprovalLineageDenialHint(DefaultName, claudePermAccept, false, DefaultName, claudePermInherit)
	if strings.Contains(accept, claudePermAuto) {
		t.Fatalf("must not suggest %q to a parent that cannot delegate it: %q", claudePermAuto, accept)
	}
	if !strings.Contains(accept, claudePermAccept) {
		t.Fatalf("hint must name the widest delegable mode %q, got %q", claudePermAccept, accept)
	}

	// Every mode the hint can name must genuinely pass the gate.
	for _, parent := range []struct {
		harness, policy string
		autoReview      bool
	}{
		{CodexName, ApprovalNever, false},
		{CodexName, ApprovalUntrusted, false},
		{CodexName, ApprovalOnRequest, true},
		{DefaultName, claudePermAccept, false},
		{DefaultName, claudePermPlan, false},
		{DefaultName, claudePermDontAsk, false},
	} {
		if mode := widestAllowedClaudeChildMode(parent.harness, parent.policy, parent.autoReview); mode != "" {
			if !ApprovalLineageAllowed(parent.harness, parent.policy, parent.autoReview, DefaultName, mode, false) {
				t.Fatalf("parent %s/%s was told to use %q, which the gate denies", parent.harness, parent.policy, mode)
			}
		}
	}

	if got := ApprovalLineageDenialHint(CodexName, ApprovalNever, false, DefaultName, claudePermBypass); got == "" {
		t.Fatal("bypass denial must explain that only an equal parent or a human can mint it")
	}
	// No misleading Claude-shaped advice for a Codex child or a provable mode.
	if got := ApprovalLineageDenialHint(CodexName, ApprovalNever, false, CodexName, ApprovalNever); got != "" {
		t.Fatalf("codex child needs no claude hint, got %q", got)
	}
	if got := ApprovalLineageDenialHint(CodexName, ApprovalNever, false, DefaultName, claudePermAuto); got != "" {
		t.Fatalf("a provable mode needs no hint, got %q", got)
	}
}
