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
		{"codex baseline to claude default", CodexName, ApprovalOnRequest, false, DefaultName, claudePermDefault, false, true},
		{"claude auto to codex never", DefaultName, claudePermAuto, false, CodexName, ApprovalNever, false, true},
		{"claude inherit to codex never", DefaultName, claudePermInherit, false, CodexName, ApprovalNever, false, true},

		// --- TCL-576 required allows: an inherit parent is not spawn-crippled ---
		{"claude inherit to claude inherit", DefaultName, claudePermInherit, false, DefaultName, claudePermInherit, false, true},
		{"claude inherit to claude auto", DefaultName, claudePermInherit, false, DefaultName, claudePermAuto, false, true},
		{"claude inherit to claude plan", DefaultName, claudePermInherit, false, DefaultName, claudePermPlan, false, true},
		{"claude accept edits to identical shape", DefaultName, claudePermAccept, false, DefaultName, claudePermAccept, false, true},
		{"claude auto to identical shape", DefaultName, claudePermAuto, false, DefaultName, claudePermAuto, false, true},

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
}

// A denial must name the way out, not just the refusal.
func TestApprovalLineageDenialHint(t *testing.T) {
	inherit := ApprovalLineageDenialHint(DefaultName, claudePermInherit)
	if !strings.Contains(inherit, claudePermAuto) {
		t.Fatalf("inherit hint must point at %q, got %q", claudePermAuto, inherit)
	}
	if got := ApprovalLineageDenialHint(DefaultName, claudePermBypass); got == "" {
		t.Fatal("bypass denial must explain that only an equal parent or a human can mint it")
	}
	// No misleading Claude-shaped advice for a Codex child or a provable mode.
	if got := ApprovalLineageDenialHint(CodexName, ApprovalNever); got != "" {
		t.Fatalf("codex child needs no claude hint, got %q", got)
	}
	if got := ApprovalLineageDenialHint(DefaultName, claudePermAuto); got != "" {
		t.Fatalf("a provable mode needs no hint, got %q", got)
	}
}
