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
		{"claude inherit cannot mint codex never", DefaultName, claudePermInherit, false, CodexName, ApprovalNever, false, false},
		// Parent inherit is an unknown live posture and receives only its proven
		// lower bound. Child inherit is charged its non-bypass upper bound.
		{"claude inherit cannot mint codex guardian", DefaultName, claudePermInherit, false, CodexName, ApprovalOnRequest, true, false},
		{"codex guardian to claude inherit", CodexName, ApprovalOnRequest, true, DefaultName, claudePermInherit, false, true},

		// --- An inherit parent may delegate only proven baseline postures ---
		{"claude inherit continues claude inherit", DefaultName, claudePermInherit, false, DefaultName, claudePermInherit, false, true},
		{"claude inherit cannot mint claude auto", DefaultName, claudePermInherit, false, DefaultName, claudePermAuto, false, false},
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

		// --- OpenCode postures share the same capability axes ---
		{"opencode deny to opencode ask", OpenCodeName, OpenCodeApprovalDeny, false, OpenCodeName, OpenCodeApprovalAsk, false, true},
		{"opencode ask to opencode deny", OpenCodeName, OpenCodeApprovalAsk, false, OpenCodeName, OpenCodeApprovalDeny, false, true},
		{"opencode deny cannot mint allow tools", OpenCodeName, OpenCodeApprovalDeny, false, OpenCodeName, OpenCodeApprovalAllowTools, false, false},
		{"opencode allow tools to same", OpenCodeName, OpenCodeApprovalAllowTools, false, OpenCodeName, OpenCodeApprovalAllowTools, false, true},
		{"opencode allow tools can mint claude accept edits", OpenCodeName, OpenCodeApprovalAllowTools, false, DefaultName, claudePermAccept, false, true},
		{"opencode allow tools cannot mint claude auto", OpenCodeName, OpenCodeApprovalAllowTools, false, DefaultName, claudePermAuto, false, false},
		{"opencode allow tools cannot mint codex never", OpenCodeName, OpenCodeApprovalAllowTools, false, CodexName, ApprovalNever, false, false},
		{"claude accept edits can mint opencode allow tools", DefaultName, claudePermAccept, false, OpenCodeName, OpenCodeApprovalAllowTools, false, true},
		{"codex never can mint opencode allow tools", CodexName, ApprovalNever, false, OpenCodeName, OpenCodeApprovalAllowTools, false, true},

		// --- Copilot postures are projected onto the same axes (TCL-973) ---
		// `allow-tools` runs arbitrary commands with no human in the loop
		// (Copilot's gate is per-command risk classification, not a tool
		// allowlist), so it is approvalAutoInSandbox — the Codex `never` /
		// Claude `auto` shape, and interchangeable with them in both directions.
		{"copilot allow-tools to same", CopilotName, CopilotApprovalAllowTools, false, CopilotName, CopilotApprovalAllowTools, false, true},
		{"copilot allow-tools to codex never", CopilotName, CopilotApprovalAllowTools, false, CodexName, ApprovalNever, false, true},
		{"copilot allow-tools to claude auto", CopilotName, CopilotApprovalAllowTools, false, DefaultName, claudePermAuto, false, true},
		{"copilot allow-tools to opencode allow tools", CopilotName, CopilotApprovalAllowTools, false, OpenCodeName, OpenCodeApprovalAllowTools, false, true},
		{"codex never to copilot allow-tools", CodexName, ApprovalNever, false, CopilotName, CopilotApprovalAllowTools, false, true},
		{"claude auto to copilot allow-tools", DefaultName, claudePermAuto, false, CopilotName, CopilotApprovalAllowTools, false, true},
		// ...and it must not be mintable by a parent that has to ask a human
		// before every non-edit command.
		{"claude accept edits cannot mint copilot allow-tools", DefaultName, claudePermAccept, false, CopilotName, CopilotApprovalAllowTools, false, false},
		{"opencode allow tools cannot mint copilot allow-tools", OpenCodeName, OpenCodeApprovalAllowTools, false, CopilotName, CopilotApprovalAllowTools, false, false},
		{"codex untrusted cannot mint copilot allow-tools", CodexName, ApprovalUntrusted, false, CopilotName, CopilotApprovalAllowTools, false, false},
		// `inherit` is the dual bound: a parent gets only the baseline it can
		// prove, a child is charged the broadest posture a Copilot config could
		// turn out to hold. So an inherit parent delegates almost nothing, and
		// an inherit CHILD needs a human — including from an allow-tools parent.
		{"copilot inherit cannot mint copilot allow-tools", CopilotName, CopilotApprovalInherit, false, CopilotName, CopilotApprovalAllowTools, false, false},
		{"copilot inherit cannot mint claude auto", CopilotName, CopilotApprovalInherit, false, DefaultName, claudePermAuto, false, false},
		{"copilot inherit to claude plan", CopilotName, CopilotApprovalInherit, false, DefaultName, claudePermPlan, false, true},
		{"copilot inherit to codex untrusted", CopilotName, CopilotApprovalInherit, false, CodexName, ApprovalUntrusted, false, true},
		{"copilot allow-tools cannot mint copilot inherit", CopilotName, CopilotApprovalAllowTools, false, CopilotName, CopilotApprovalInherit, false, false},
		{"codex never cannot mint copilot inherit", CodexName, ApprovalNever, false, CopilotName, CopilotApprovalInherit, false, false},
		// Unlike Claude, there is NO inherit→inherit continuation exception.
		// That exception exists to keep the ordinary recursive Claude workflow
		// usable; Copilot has no such established workflow, and adding one would
		// mean crediting an unprovable posture on a harness whose in-pane
		// commands and remembered answers can widen it further.
		{"copilot inherit does not continue to copilot inherit", CopilotName, CopilotApprovalInherit, false, CopilotName, CopilotApprovalInherit, false, false},
		// Only a posture that already holds unreviewed capability can mint one.
		{"claude bypass can mint copilot inherit", DefaultName, claudePermBypass, false, CopilotName, CopilotApprovalInherit, false, true},
		{"claude bypass can mint copilot allow-tools", DefaultName, claudePermBypass, false, CopilotName, CopilotApprovalAllowTools, false, true},

		// `yolo` (TCL-1010) is Copilot's bypassPermissions: every prompt gone,
		// no reviewer of any kind, and — outside tclaude-layer — no file
		// boundary left, since Copilot's directory check was the only one and
		// its built-in edits are not OS-confined. Claude auto is the deliberate
		// cross-harness exception: its supervisor can approve the same operations,
		// while sandbox lineage independently bounds the child's file access.
		{"copilot allow-tools cannot mint copilot yolo", CopilotName, CopilotApprovalAllowTools, false, CopilotName, CopilotApprovalYolo, false, false},
		{"codex never cannot mint copilot yolo", CodexName, ApprovalNever, false, CopilotName, CopilotApprovalYolo, false, false},
		{"claude auto can mint copilot yolo", DefaultName, claudePermAuto, false, CopilotName, CopilotApprovalYolo, false, true},
		{"malformed claude auto-review cannot mint copilot yolo", DefaultName, claudePermAuto, true, CopilotName, CopilotApprovalYolo, false, false},
		{"claude bypass can mint copilot yolo", DefaultName, claudePermBypass, false, CopilotName, CopilotApprovalYolo, false, true},
		{"copilot yolo to same", CopilotName, CopilotApprovalYolo, false, CopilotName, CopilotApprovalYolo, false, true},
		// A yolo parent holds everything, so it can delegate every narrower
		// posture — including the ones an allow-tools parent cannot prove.
		{"copilot yolo can mint copilot allow-tools", CopilotName, CopilotApprovalYolo, false, CopilotName, CopilotApprovalAllowTools, false, true},
		{"copilot yolo can mint copilot inherit", CopilotName, CopilotApprovalYolo, false, CopilotName, CopilotApprovalInherit, false, true},
		{"copilot yolo can mint claude bypass", CopilotName, CopilotApprovalYolo, false, DefaultName, claudePermBypass, false, true},
		{"copilot yolo auto-review parent is malformed", CopilotName, CopilotApprovalYolo, true, CopilotName, CopilotApprovalYolo, false, false},

		// --- Malformed / unclassifiable postures fail closed ---
		{"legacy blank copilot parent fails closed", CopilotName, "", false, CopilotName, CopilotApprovalAllowTools, false, false},
		{"legacy blank copilot child fails closed", DefaultName, claudePermBypass, false, CopilotName, "", false, false},
		{"unknown copilot policy fails closed", CopilotName, "allow-all", false, CopilotName, CopilotApprovalAllowTools, false, false},
		{"copilot auto-review parent is malformed", CopilotName, CopilotApprovalAllowTools, true, CopilotName, CopilotApprovalAllowTools, false, false},
		{"copilot auto-review child is malformed", DefaultName, claudePermBypass, false, CopilotName, CopilotApprovalAllowTools, true, false},

		{"legacy blank codex parent fails closed", CodexName, "", false, CodexName, ApprovalNever, false, false},
		{"legacy blank claude parent fails closed", DefaultName, "", false, DefaultName, claudePermAuto, false, false},
		{"legacy blank claude child fails closed", DefaultName, claudePermBypass, false, DefaultName, "", false, false},
		{"legacy blank opencode parent fails closed", OpenCodeName, "", false, OpenCodeName, OpenCodeApprovalDeny, false, false},
		{"legacy blank opencode child fails closed", DefaultName, claudePermBypass, false, OpenCodeName, "", false, false},
		{"unknown opencode policy fails closed", OpenCodeName, "anything", false, OpenCodeName, OpenCodeApprovalDeny, false, false},
		{"opencode auto-review parent is malformed", OpenCodeName, OpenCodeApprovalAllowTools, true, OpenCodeName, OpenCodeApprovalDeny, false, false},
		{"opencode auto-review child is malformed", DefaultName, claudePermBypass, false, OpenCodeName, OpenCodeApprovalDeny, true, false},
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
	if !ApprovalLineageAllowed("", claudePermInherit, false, "", claudePermPlan, false) {
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

	openCode := ApprovalLineageDenialHint(OpenCodeName, OpenCodeApprovalDeny, false,
		OpenCodeName, OpenCodeApprovalAllowTools)
	if !strings.Contains(openCode, OpenCodeApprovalAsk) {
		t.Fatalf("OpenCode allow-tools denial must name a delegable human-gated posture, got %q", openCode)
	}
	if got := ApprovalLineageDenialHint(OpenCodeName, OpenCodeApprovalDeny, false,
		OpenCodeName, OpenCodeApprovalDeny); got != "" {
		t.Fatalf("a provable OpenCode mode needs no hint, got %q", got)
	}
}

// The Copilot hint has the same job as the Claude `inherit` one: name a way out
// that actually works, and say nothing when there isn't one. The refused case is
// always an `inherit` CHILD, because that is the only Copilot posture whose
// effective breadth cannot be proven at spawn time.
func TestCopilotApprovalLineageDenialHint(t *testing.T) {
	// A parent holding full in-sandbox execution can delegate allow-tools, so
	// the hint names it — and the gate must really allow what it names.
	fromCapable := ApprovalLineageDenialHint(CodexName, ApprovalNever, false,
		CopilotName, CopilotApprovalInherit)
	if !strings.Contains(fromCapable, CopilotApprovalAllowTools) {
		t.Fatalf("hint must point at %q, got %q", CopilotApprovalAllowTools, fromCapable)
	}
	if !ApprovalLineageAllowed(CodexName, ApprovalNever, false, CopilotName, CopilotApprovalAllowTools, false) {
		t.Fatal("the hint named a posture the gate denies")
	}
	// An allow-tools Copilot parent is in the same position: it cannot prove an
	// inherit child is no broader than itself, but it CAN mint allow-tools.
	fromCopilot := ApprovalLineageDenialHint(CopilotName, CopilotApprovalAllowTools, false,
		CopilotName, CopilotApprovalInherit)
	if !strings.Contains(fromCopilot, CopilotApprovalAllowTools) {
		t.Fatalf("an allow-tools parent must be told it can mint %q, got %q", CopilotApprovalAllowTools, fromCopilot)
	}
	// A parent that can delegate nothing Copilot offers must be told a human is
	// needed rather than pointed at a mode that earns a second 403.
	fromNarrow := ApprovalLineageDenialHint(DefaultName, claudePermPlan, false,
		CopilotName, CopilotApprovalInherit)
	if strings.Contains(fromNarrow, CopilotApprovalAllowTools) {
		t.Fatalf("must not suggest %q to a parent that cannot delegate it: %q", CopilotApprovalAllowTools, fromNarrow)
	}
	if !strings.Contains(fromNarrow, "human") {
		t.Fatalf("hint must say a human is needed, got %q", fromNarrow)
	}
	// allow-tools is the narrowest token; a parent that cannot delegate it has
	// no narrower Copilot posture to be pointed at, so the hint stays silent
	// rather than inventing advice.
	if got := ApprovalLineageDenialHint(DefaultName, claudePermPlan, false,
		CopilotName, CopilotApprovalAllowTools); got != "" {
		t.Fatalf("no Copilot posture is narrower than allow-tools; hint should be empty, got %q", got)
	}

	// A denied `yolo` child gets its own hint, the counterpart to Claude's
	// bypassPermissions one: it names the specific loss rather than "removes
	// guardrails", and it points at the token that keeps directory access
	// scoped. Without this the widest new token would be the only Copilot
	// posture whose refusal came with no way forward.
	yolo := ApprovalLineageDenialHint(CopilotName, CopilotApprovalAllowTools, false,
		CopilotName, CopilotApprovalYolo)
	for _, want := range []string{CopilotApprovalYolo, CopilotApprovalAllowTools, "human", "directory"} {
		if !strings.Contains(yolo, want) {
			t.Fatalf("the yolo hint must contain %q, got %q", want, yolo)
		}
	}
}
