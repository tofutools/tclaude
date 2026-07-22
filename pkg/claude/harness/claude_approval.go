package harness

import (
	"fmt"
	"strings"
)

// Claude Code permission modes — the `claude --permission-mode <mode>` enum
// (verified against claude 2.1.195 `--help`), plus tclaude's `inherit`
// sentinel. This is the approval-axis counterpart to claudeSandbox: Claude Code
// has no separate "approval policy" flag — its approval behaviour IS the
// permission mode — so tclaude's harness-agnostic Approval field carries a
// permission-mode value for Claude, and claudeSpawner translates it to
// `--permission-mode`.
//
//   - inherit           : add no override — Claude uses your settings.json
//     permission rules (allow/deny/ask) as-is. NORMALIZES to
//     "" (omit), so a spawn that chooses it passes no
//     --permission-mode. No longer the default — see
//     claudeApproval below.
//   - default           : standard interactive permissions — prompts before
//     every non-read-only action.
//   - plan              : read-only planning — explores/reads but doesn't edit.
//   - acceptEdits       : auto-approve edits + common fs commands in the cwd;
//     other actions prompt.
//   - auto              : a separate supervisor model approves safe actions and
//     blocks unsafe ones; explicit ask-rules still prompt.
//   - dontAsk           : auto-DENY anything not pre-approved; never prompts.
//   - bypassPermissions : auto-approve everything (≈ --dangerously-skip-
//     permissions); cannot run as root.
const (
	claudePermInherit = "inherit"
	claudePermDefault = "default"
	claudePermPlan    = "plan"
	claudePermAccept  = "acceptEdits"
	claudePermAuto    = "auto"
	claudePermDontAsk = "dontAsk"
	claudePermBypass  = "bypassPermissions"
)

// ClaudePermissionInherit is the public spelling of tclaude's inherit
// sentinel. Session persistence and authorization layers use it without
// duplicating a harness-owned policy token.
const ClaudePermissionInherit = claudePermInherit

// claudeApproval is Claude Code's ApprovalCatalog. The default is `auto`: a
// supervisor model approves safe actions and blocks unsafe ones, so a
// tclaude-spawned (detached, unattended) Claude agent can work without waiting
// on a prompt no human is at the pane to answer.
//
// The default used to be `inherit` (= emit no `--permission-mode` and keep the
// operator's settings.json posture). That satisfied the letter of the
// *non-escalating* contract but was a poor default in practice, for two
// reasons:
//
//   - settings.json is usually an interactive posture, so an un-chosen spawn
//     inherited prompts and the detached pane blocked — the JOH-167 deadlock
//     the contract exists to prevent, reintroduced through the default.
//   - `inherit` is unknowable at spawn time, so approval lineage
//     (classifyApprovalLineage) can only credit an inherit PARENT with
//     approvalAutoBaseline while charging an inherit CHILD the broadest
//     non-bypass capability. A default-spawned agent therefore could not mint
//     capable children — every delegation hit the spawn approval guard.
//
// `auto` is still non-escalating in the sense the contract cares about: per
// TCL-92 its classifier tightens what runs INSIDE the sandbox and is not a
// boundary-escalation grant (it holds approvalAutoInSandbox, not
// approvalAutoReviewer or approvalAutoUnreviewed). It is a known, bounded
// posture rather than an unknown one, which is what makes lineage tractable.
//
// `inherit` remains selectable as an explicit per-session override for an
// operator who really does want their settings.json posture verbatim.
//
// The "in-sandbox" half of that classification is a statement about what the
// classifier does, NOT a promise that a sandbox exists: Claude's sandbox
// default is `inherit`, so an operator who configures none gets `auto` with the
// classifier as the only gate. TCL-586 settled that tclaude warns about that
// combination rather than overriding the sandbox axis —
// UnsandboxedAutonomyWarnings (claude_sandbox_effective.go) is the check, and
// it fires on the CLI, on the spawn response, and in the dashboard spawn
// dialog.
type claudeApproval struct{}

func (claudeApproval) DefaultPolicy() string { return claudePermAuto }

// ValidatePolicy normalizes and validates a requested permission mode,
// preserving the tri-state the overlay sites depend on (mirrors
// claudeSandbox.ValidateMode):
//
//   - ""      → "" (OMITTED — a higher level may fill it; if nothing does, the
//     launch boundary applies the harness default).
//   - inherit → "inherit" (ACTIVELY chosen — carried through as a first-class
//     sentinel so an overlay does NOT overwrite it; collapses to "omit the
//     --permission-mode flag" only at emission, see claudeApprovalValue).
//   - the six real modes → themselves.
//   - anything else → an error naming the set.
//
// The old behaviour collapsed inherit to "" here, making an explicit inherit
// indistinguishable from omitted so a profile/group default silently won;
// keeping inherit distinct is the fix.
func (claudeApproval) ValidatePolicy(policy string) (string, error) {
	switch p := strings.TrimSpace(policy); p {
	case "":
		return "", nil
	case claudePermInherit:
		return claudePermInherit, nil
	case claudePermDefault, claudePermPlan, claudePermAccept, claudePermAuto, claudePermDontAsk, claudePermBypass:
		return p, nil
	default:
		return "", fmt.Errorf("invalid claude permission mode %q (want %s|%s|%s|%s|%s|%s|%s)",
			policy, claudePermInherit, claudePermDefault, claudePermPlan,
			claudePermAccept, claudePermAuto, claudePermDontAsk, claudePermBypass)
	}
}

// claudeApprovalValue returns the `--permission-mode` flag value the spawner
// should emit for a validated policy, or "" when NO flag should be emitted
// (inherit / unset / unrecognized). This is where the first-class `inherit`
// sentinel collapses to "omit the flag", the LAST layer that sees it — the
// approval-axis counterpart to claudeSandboxBlock (→ nil) and
// claudeAskTimeoutValue (→ ""). Without this, an explicit-inherit spawn (now
// carried as "inherit" rather than collapsed early) would emit a bogus
// `--permission-mode inherit` Claude Code rejects.
func claudeApprovalValue(policy string) string {
	switch strings.TrimSpace(policy) {
	case claudePermDefault, claudePermPlan, claudePermAccept, claudePermAuto, claudePermDontAsk, claudePermBypass:
		return strings.TrimSpace(policy)
	default:
		return ""
	}
}

// Modes lists the selectable permission modes for spawn UIs: the `inherit`
// escape hatch first, then the six real modes roughly by ascending autonomy.
// Order is presentation only — the spawn dialog and profile editor preselect
// and tag "(recommended)" by matching DefaultPolicy() (now `auto`) against the
// option VALUE, not by position. A fresh slice each call so a caller can't
// mutate the set.
func (claudeApproval) Modes() []string {
	return []string{
		claudePermInherit, claudePermPlan, claudePermDefault,
		claudePermAccept, claudePermAuto, claudePermDontAsk, claudePermBypass,
	}
}

// claudePermissionModeHelp is the one-line description the spawn UI shows for
// each mode. Because tclaude-spawned agents run DETACHED (a tmux pane with no
// human watching), the help flags the modes that can block on a prompt no one
// can answer, or auto-deny, or remove all guardrails — the "⚠" hint the dialog
// renders in its warn colour. Keyed by mode value. (Source: Claude Code
// permission-modes docs, v2.1.195.)
var claudePermissionModeHelp = map[string]string{
	claudePermInherit: "Use your settings.json permission rules and the agentd approval popup as-is. ⚠ Whatever posture that is — usually an interactive one — a detached agent can block on a prompt no one answers, and it delegates poorly (an unknown posture can't mint capable child agents).",
	claudePermPlan:    "Read-only planning — Claude explores and proposes a plan without editing files. ⚠ Still prompts on a write, so a detached agent can block if it tries one.",
	claudePermDefault: "Standard interactive permissions — prompts before every non-read-only action. ⚠ A detached agent (no human at the pane) can block on a prompt no one answers.",
	claudePermAccept:  "Auto-approve file edits + common filesystem commands (mkdir/touch/mv/cp/rm) in the working dir; other actions prompt. ⚠ Can still block a detached agent on a non-edit prompt.",
	claudePermAuto:    "A separate supervisor model approves safe actions and blocks unsafe ones (curl|bash, force-push, prod deploys); explicit ask-rules still prompt. The most autonomous mode that keeps guardrails — well suited to a detached agent (only a rare classifier fallback can prompt).",
	claudePermDontAsk: "Auto-DENY every action not pre-approved by your allow-rules (or read-only); never prompts. ⚠ A detached agent silently fails anything you haven't pre-allowed.",
	claudePermBypass:  "⚠ Bypass ALL permission checks (≈ --dangerously-skip-permissions): auto-approve everything. No deadlocks but no guardrails — use only in a trusted/sandboxed context; cannot run as root.",
}

// ModeHelp returns a one-line description of a permission mode for spawn UIs,
// or "" for an unrecognized mode. The inherit help is keyed under its token
// even though ValidatePolicy collapses it to "" (the dashboard renders help off
// the raw Modes() tokens, not the validated value).
func (claudeApproval) ModeHelp(policy string) string {
	return claudePermissionModeHelp[strings.TrimSpace(policy)]
}
