package harness

import (
	"fmt"
	"slices"
	"strings"
)

// pi's approval catalog. pi itself never asks before a tool call, so tclaude's
// levers at launch are which tools exist and its own permission extension:
//
//   - auto      — pi's native posture: the default tool set (read, bash, edit,
//     write) runs with no human. This is the same unattended-blanket class
//     as Copilot's and Gemini's yolo.
//   - ask       — tclaude's permission extension (piextension/
//     tclaude-pi-permissions.ts) asks the human before every tool call that
//     is not read-only, and blocks one the human declines.
//   - read-only — `--tools read,grep,find,ls`: no shell and no file writes,
//     so nothing the model does executes or mutates on its own.
//
// Blank emits nothing, which IS auto: pi has no configuration that adds a
// prompt, so an absent posture is never narrower than auto.
const (
	PiApprovalAuto     = "auto"
	PiApprovalAsk      = "ask"
	PiApprovalReadOnly = "read-only"
)

// piReadOnlyTools is the allowlist rendered for read-only: pi's read-only
// built-ins. `--tools` with plain names replaces the whole selection, so
// extension and MCP tools are dropped too unless an entry names them.
var piReadOnlyTools = []string{"read", "grep", "find", "ls"}

var piApprovalModes = []string{PiApprovalAuto, PiApprovalAsk, PiApprovalReadOnly}

var piApprovalModeHelp = map[string]string{
	PiApprovalAuto: "pi's native posture: shell commands and file edits run with no human prompt; " +
		"pair it with tclaude's sandbox (--sandbox-impl tclaude-layer), which is the only boundary",
	PiApprovalAsk: "a human approves every tool call that is not read-only (shell commands, file edits, " +
		"MCP tools not marked read-only) in a dialog in the pane; a declined call is blocked and the model is told",
	PiApprovalReadOnly: "only pi's read-only tools (read, grep, find, ls): no shell, no file writes",
}

// piApproval is pi's ApprovalCatalog. DefaultPolicy is auto because it is the
// only posture in which pi does its job unattended; ask parks the pane on a
// dialog until a human answers, so it is never chosen for one.
type piApproval struct{}

func (piApproval) DefaultPolicy() string { return PiApprovalAuto }
func (piApproval) Modes() []string       { return slices.Clone(piApprovalModes) }

func (piApproval) ValidatePolicy(policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	if policy == "" || slices.Contains(piApprovalModes, policy) {
		return policy, nil
	}
	return "", fmt.Errorf("invalid pi approval mode %q (want %s)", policy, strings.Join(piApprovalModes, "|"))
}

func (piApproval) ModeHelp(policy string) string {
	return piApprovalModeHelp[strings.TrimSpace(policy)]
}

// piApprovalArgs renders a validated policy as pi flags. Blank and auto emit
// nothing, and so does ask: its gate is the permission extension the spawner
// loads (piSpawner.BuildCommand).
func piApprovalArgs(policy string) []string {
	if strings.TrimSpace(policy) == PiApprovalReadOnly {
		return []string{"--tools", strings.Join(piReadOnlyTools, ",")}
	}
	return nil
}
