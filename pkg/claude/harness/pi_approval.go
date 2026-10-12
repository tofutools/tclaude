package harness

import (
	"fmt"
	"slices"
	"strings"
)

// pi's approval catalog. pi itself never asks before a tool call, so the only
// lever tclaude has at launch is which tools exist:
//
//   - auto      — pi's native posture: the default tool set (read, bash, edit,
//     write) runs with no human. This is the same unattended-blanket class
//     as Copilot's and Gemini's yolo.
//   - read-only — `--tools read,grep,find,ls`: no shell and no file writes,
//     so nothing the model does executes or mutates on its own.
//
// Blank emits nothing, which IS auto: pi has no configuration that adds a
// prompt, so an absent posture is never narrower than auto. A human-gated
// posture needs a permission extension and is a later phase.
const (
	PiApprovalAuto     = "auto"
	PiApprovalReadOnly = "read-only"
)

// piReadOnlyTools is the allowlist rendered for read-only: pi's read-only
// built-ins. `--tools` with plain names replaces the whole selection, so
// extension and MCP tools are dropped too unless an entry names them.
var piReadOnlyTools = []string{"read", "grep", "find", "ls"}

var piApprovalModes = []string{PiApprovalAuto, PiApprovalReadOnly}

var piApprovalModeHelp = map[string]string{
	PiApprovalAuto: "pi's native posture: shell commands and file edits run with no human prompt; " +
		"pair it with tclaude's sandbox (--sandbox-impl tclaude-layer), which is the only boundary",
	PiApprovalReadOnly: "only pi's read-only tools (read, grep, find, ls): no shell, no file writes",
}

// piApproval is pi's ApprovalCatalog. DefaultPolicy is auto because it is the
// only posture in which pi does its job unattended, and pi has no prompting
// posture that could deadlock a detached pane anyway.
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
// nothing.
func piApprovalArgs(policy string) []string {
	if strings.TrimSpace(policy) == PiApprovalReadOnly {
		return []string{"--tools", strings.Join(piReadOnlyTools, ",")}
	}
	return nil
}
