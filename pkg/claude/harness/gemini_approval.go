package harness

import (
	"fmt"
	"slices"
	"strings"
)

// Gemini CLI's approval catalog: its own `--approval-mode`, rendered as one
// flag. The semantics below were read from the built-in policy files at
// GeminiPinnedVersion (packages/core/src/policy/policies/*.toml), not observed
// from a live pane:
//
//   - default   — edits (replace, write_file), run_shell_command, web_fetch and
//     skill activation all ask a human. A detached pane parks on the first
//     such tool (the dashboard shows it as awaiting permission).
//   - auto_edit — edits inside the workspace (an `allowed-path` safety checker
//     guards them) and web_fetch run unattended; shell commands still ask.
//   - yolo      — every tool call is allowed, shell included. The one thing
//     still put to a human is Gemini's ask_user tool, which is a question
//     rather than a permission. Gemini's own file tools stay bound to the
//     workspace directories; shell commands are not.
//   - plan      — read-only: every mutating tool is denied.
//
// Gemini silently drops yolo and auto_edit back to default in a folder it
// does not trust, which is one more reason --trust-dir exists.
//
// `inherit` emits no flag, leaving settings.json `general.defaultApprovalMode`
// (or Gemini's own default) to decide.
const (
	GeminiApprovalInherit  = "inherit"
	GeminiApprovalDefault  = "default"
	GeminiApprovalAutoEdit = "auto_edit"
	GeminiApprovalYolo     = "yolo"
	GeminiApprovalPlan     = "plan"
)

// geminiApprovalModes is the ordered set shared by validation, profiles and
// the dashboard selector: the unattended default first, the prompting
// postures after it, inherit last as the "decided elsewhere" escape hatch.
var geminiApprovalModes = []string{
	GeminiApprovalYolo, GeminiApprovalAutoEdit, GeminiApprovalDefault,
	GeminiApprovalPlan, GeminiApprovalInherit,
}

// geminiApproval is Gemini CLI's ApprovalCatalog.
//
// DefaultPolicy is `yolo` for the reason Copilot's is `allow-tools`: it is the
// only Gemini posture a detached pane cannot deadlock in, and the catalog
// contract demands a non-deadlocking default for an unattended agent. Its risk
// shape is also Copilot's `allow-tools`: arbitrary shell commands with no human,
// while the harness's own file tools stay workspace-bound. So, like that
// default, it carries no spawn-time warning; what confines the shell is the
// sandbox axis (--sandbox-impl tclaude-layer), and the mode help says so.
type geminiApproval struct{}

func (geminiApproval) DefaultPolicy() string { return GeminiApprovalYolo }

func (geminiApproval) Modes() []string { return slices.Clone(geminiApprovalModes) }

func (geminiApproval) ValidatePolicy(policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	if policy == "" || slices.Contains(geminiApprovalModes, policy) {
		return policy, nil
	}
	return "", fmt.Errorf("invalid gemini approval mode %q (want %s)",
		policy, strings.Join(geminiApprovalModes, "|"))
}

// Mode help follows the catalog convention: everything from the single "⚠"
// onward stays visible when the spawn UI collapses the rest.
var geminiApprovalModeHelp = map[string]string{
	GeminiApprovalYolo: "Every tool call runs without confirmation, shell commands included " +
		"(--approval-mode=yolo). Gemini's ask_user tool still asks a human. " +
		"⚠ Shell commands are not confined by Gemini itself: without --sandbox-impl " +
		"tclaude-layer the agent can do anything the pane's user can. Gemini also drops " +
		"to `default` in a folder it does not trust.",
	GeminiApprovalAutoEdit: "File edits inside the workspace and web fetches run without " +
		"confirmation; shell commands still ask. " +
		"⚠ A detached agent waits for a human on its first shell command.",
	GeminiApprovalDefault: "Gemini's prompting posture: edits, shell commands, web fetches and " +
		"skills all ask a human. ⚠ A detached agent waits on its first such tool.",
	GeminiApprovalPlan: "Read-only planning: every mutating tool is denied. " +
		"⚠ The agent cannot change anything until a human switches the mode in its pane.",
	GeminiApprovalInherit: "Emit no --approval-mode; your Gemini settings " +
		"(general.defaultApprovalMode) decide. " +
		"⚠ tclaude cannot tell from outside the pane whether a detached agent will wait for a human.",
}

func (geminiApproval) ModeHelp(policy string) string {
	return geminiApprovalModeHelp[strings.TrimSpace(policy)]
}

// geminiApprovalArg renders a validated policy as Gemini's flag, attached
// with `=` like every value tclaude renders for Gemini. Blank and inherit emit
// nothing.
func geminiApprovalArg(policy string) string {
	switch policy = strings.TrimSpace(policy); policy {
	case "", GeminiApprovalInherit:
		return ""
	default:
		return "--approval-mode=" + policy
	}
}
