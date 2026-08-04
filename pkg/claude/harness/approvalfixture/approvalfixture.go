// Package approvalfixture holds the canonical table of approval-posture
// RECONSTRUCTION cases: what each resume/relaunch surface must produce for a
// given harness and recorded approval input.
//
// It exists because the surfaces live in packages that cannot import each
// other's tests — the CLI resume path in pkg/claude/conv, the daemon relaunch
// paths in pkg/claude/agentd, the rule itself in pkg/claude/harness. Each of
// them asserts against THIS table, so a surface that starts pinning a blank row
// to a historical value, or stops re-validating a recorded one, fails its own
// package's test instead of silently diverging from the others. See TCL-990.
package approvalfixture

import "github.com/tofutools/tclaude/pkg/claude/harness"

// Case is one (harness, recorded input) pair and the posture every surface must
// reconstruct from it.
type Case struct {
	// Name identifies the case in subtest output.
	Name string
	// Harness is the harness the conversation is tagged with.
	Harness string
	// Recorded is the approval input the conversation recorded. "" means no
	// input was recorded at all — NOT a recorded empty posture.
	Recorded string
	// Want is the posture reconstruction must yield.
	Want string
	// Reresolved reports whether Want came from current config (Recorded was
	// absent) rather than from the record.
	Reresolved bool
}

// Cases is the canonical table. The absent-input rows are the load-bearing
// ones: each of them names a harness default that a surface previously pinned
// to some stricter historical value instead.
func Cases() []Case {
	return []Case{
		// Absent input → re-resolve under current config.
		{
			Name:    "claude blank re-resolves to the current default",
			Harness: harness.DefaultName, Recorded: "",
			Want: "auto", Reresolved: true,
		},
		{
			Name:    "codex blank re-resolves to the current default",
			Harness: harness.CodexName, Recorded: "",
			Want: harness.ApprovalNever, Reresolved: true,
		},
		{
			Name:    "copilot blank re-resolves to the current default",
			Harness: harness.CopilotName, Recorded: "",
			Want: harness.CopilotApprovalAllowTools, Reresolved: true,
		},
		{
			Name:    "opencode blank re-resolves to the current default",
			Harness: harness.OpenCodeName, Recorded: "",
			Want: harness.OpenCodeApprovalDeny, Reresolved: true,
		},

		// Explicitly recorded input → reproduced exactly, whether that is
		// broader or narrower than the current default.
		{
			Name:    "claude explicit inherit is reproduced",
			Harness: harness.DefaultName, Recorded: harness.ClaudePermissionInherit,
			Want: harness.ClaudePermissionInherit,
		},
		{
			Name:    "claude explicit plan is reproduced",
			Harness: harness.DefaultName, Recorded: "plan", Want: "plan",
		},
		{
			Name:    "codex explicit untrusted is reproduced",
			Harness: harness.CodexName, Recorded: harness.ApprovalUntrusted,
			Want: harness.ApprovalUntrusted,
		},
		{
			Name:    "codex explicit on-request is reproduced",
			Harness: harness.CodexName, Recorded: harness.ApprovalOnRequest,
			Want: harness.ApprovalOnRequest,
		},
		{
			Name:    "copilot explicit inherit is reproduced",
			Harness: harness.CopilotName, Recorded: harness.CopilotApprovalInherit,
			Want: harness.CopilotApprovalInherit,
		},
		{
			Name:    "opencode explicit ask is reproduced",
			Harness: harness.OpenCodeName, Recorded: harness.OpenCodeApprovalAsk,
			Want: harness.OpenCodeApprovalAsk,
		},
	}
}
