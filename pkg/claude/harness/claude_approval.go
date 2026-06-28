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
//     permission rules (allow/deny/ask) as-is. The default;
//     NORMALIZES to "" (omit). Preserves today's behaviour
//     (an un-chosen Claude spawn passes no --permission-mode).
//   - default           : standard interactive permissions — prompts before
//     every non-read-only action.
//   - plan              : read-only planning — explores/reads but doesn't edit.
//   - acceptEdits       : auto-approve edits + common fs commands in the cwd;
//     other actions prompt.
//   - auto              : a classifier model auto-approves safe actions and
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

// claudeApproval is Claude Code's ApprovalCatalog. The default is `inherit`:
// like the sandbox default, a tclaude-spawned Claude agent keeps whatever
// permission posture the operator configured in settings.json (the agentd
// approval popup + the operator's allow/deny rules), never silently overridden.
// `inherit` normalizes to "" so an un-chosen spawn passes no `--permission-mode`
// — exactly today's behaviour. The explicit modes are the per-session override.
//
// Note the contract tension: ApprovalCatalog.DefaultPolicy is documented as a
// *non-escalating* default (Codex returns `never`). For Claude the operator's
// settings.json IS that posture — the agentd approval flow already handles
// prompts out-of-band — so `inherit` (= don't touch it) is the right
// non-escalating choice here, not a forced mode.
type claudeApproval struct{}

func (claudeApproval) DefaultPolicy() string { return claudePermInherit }

// ValidatePolicy normalizes and validates a requested permission mode. Both ""
// and `inherit` return "" (no override — mirrors claudeSandbox.ValidateMode).
// The six real modes return themselves; anything else errors naming the set.
func (claudeApproval) ValidatePolicy(policy string) (string, error) {
	switch strings.TrimSpace(policy) {
	case "", claudePermInherit:
		return "", nil
	case claudePermDefault, claudePermPlan, claudePermAccept, claudePermAuto, claudePermDontAsk, claudePermBypass:
		return strings.TrimSpace(policy), nil
	default:
		return "", fmt.Errorf("invalid claude permission mode %q (want %s|%s|%s|%s|%s|%s|%s)",
			policy, claudePermInherit, claudePermDefault, claudePermPlan,
			claudePermAccept, claudePermAuto, claudePermDontAsk, claudePermBypass)
	}
}

// Modes lists the selectable permission modes for spawn UIs: inherit (the
// recommended default) first, then the six real modes roughly by ascending
// autonomy. A fresh slice each call so a caller can't mutate the set.
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
	claudePermInherit: "Recommended. No per-session override — Claude uses your settings.json permission rules (allow / deny / ask) and the agentd approval popup as-is.",
	claudePermPlan:    "Read-only planning — Claude explores and proposes a plan without editing files. ⚠ Still prompts on a write, so a detached agent can block if it tries one.",
	claudePermDefault: "Standard interactive permissions — prompts before every non-read-only action. ⚠ A detached agent (no human at the pane) can block on a prompt no one answers.",
	claudePermAccept:  "Auto-approve file edits + common filesystem commands (mkdir/touch/mv/cp/rm) in the working dir; other actions prompt. ⚠ Can still block a detached agent on a non-edit prompt.",
	claudePermAuto:    "A classifier model auto-approves safe actions and blocks unsafe ones (curl|bash, force-push, prod deploys); explicit ask-rules still prompt. Mostly safe for a detached agent (rare fallback prompts).",
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
