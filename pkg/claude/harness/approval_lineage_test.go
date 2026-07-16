package harness

import "testing"

func TestApprovalLineageAllowedMatrix(t *testing.T) {
	tests := []struct {
		name                        string
		parentHarness, parentPolicy string
		parentAutoReview            bool
		childHarness, childPolicy   string
		childAutoReview             bool
		want                        bool
	}{
		{"codex baseline cannot delegate settings-driven claude default", CodexName, ApprovalOnRequest, false, DefaultName, claudePermDefault, false, false},
		{"codex baseline to claude bypass", CodexName, ApprovalNever, false, DefaultName, claudePermBypass, false, false},
		{"codex guardian cannot delegate settings-driven claude auto", CodexName, ApprovalOnRequest, true, DefaultName, claudePermAuto, false, false},
		{"codex never plus idle guardian cannot mint claude auto", CodexName, ApprovalNever, true, DefaultName, claudePermAuto, false, false},
		{"codex never plus idle guardian cannot delegate settings-driven claude", CodexName, ApprovalNever, true, DefaultName, claudePermDefault, false, false},
		{"codex guardian to claude accept edits is incomparable", CodexName, ApprovalOnRequest, true, DefaultName, claudePermAccept, false, false},
		{"claude accept edits to codex guardian is incomparable", DefaultName, claudePermAccept, false, CodexName, ApprovalOnRequest, true, false},
		{"claude accept edits cannot delegate settings-driven claude", DefaultName, claudePermAccept, false, DefaultName, claudePermAccept, false, false},
		{"claude auto cannot delegate codex in-sandbox execution", DefaultName, claudePermAuto, false, CodexName, ApprovalUntrusted, true, false},
		{"claude bypass to any posture", DefaultName, claudePermBypass, false, DefaultName, claudePermInherit, false, true},
		{"claude bypass to codex guardian", DefaultName, claudePermBypass, false, CodexName, ApprovalOnRequest, true, true},
		{"claude inherit cannot delegate settings-driven inherit", DefaultName, claudePermInherit, false, DefaultName, claudePermInherit, false, false},
		{"claude inherit cannot delegate codex in-sandbox execution", DefaultName, claudePermInherit, false, CodexName, ApprovalNever, false, false},
		{"claude inherit to claude auto", DefaultName, claudePermInherit, false, DefaultName, claudePermAuto, false, false},
		{"codex baseline to codex baseline", CodexName, ApprovalOnRequest, false, CodexName, ApprovalNever, false, true},
		{"codex untrusted cannot delegate sandbox-auto never", CodexName, ApprovalUntrusted, false, CodexName, ApprovalNever, false, false},
		{"codex untrusted to untrusted", CodexName, ApprovalUntrusted, false, CodexName, ApprovalUntrusted, false, true},
		{"codex baseline cannot enable classifier", CodexName, ApprovalNever, false, CodexName, ApprovalOnRequest, true, false},
		{"codex untrusted classifier cannot delegate sandbox-auto never", CodexName, ApprovalUntrusted, true, CodexName, ApprovalNever, false, false},
		{"codex untrusted classifier to same", CodexName, ApprovalUntrusted, true, CodexName, ApprovalUntrusted, true, true},
		{"codex classifier to codex classifier", CodexName, ApprovalOnRequest, true, CodexName, ApprovalUntrusted, true, true},
		{"legacy blank parent fails closed", CodexName, "", false, CodexName, ApprovalNever, false, false},
		{"invalid claude auto-review fails closed", DefaultName, claudePermDefault, true, DefaultName, claudePermDefault, false, false},
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
