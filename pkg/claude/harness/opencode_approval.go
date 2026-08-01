package harness

import (
	"fmt"
	"strings"
)

const (
	// OpenCodeApprovalDeny is the unattended default: reads inside the selected
	// reach and the access-control tool baseline are allowed, while edits, web,
	// and unlisted tools are denied rather than left waiting for a human.
	OpenCodeApprovalDeny = "deny"

	// OpenCodeApprovalAsk lets a human approve representable edits and web
	// tools. The access-control tool baseline remains enabled. Sandbox off leaves
	// OpenCode's native permission posture untouched.
	OpenCodeApprovalAsk = "ask"

	// OpenCodeApprovalAllowTools automatically permits representable edits and
	// audited web tools. The access-control tool baseline remains enabled.
	// Sandbox off leaves OpenCode's native permission posture untouched.
	OpenCodeApprovalAllowTools = "allow-tools"
)

type openCodeApproval struct{}

func (openCodeApproval) DefaultPolicy() string { return OpenCodeApprovalDeny }

func (openCodeApproval) Modes() []string {
	return []string{OpenCodeApprovalDeny, OpenCodeApprovalAsk, OpenCodeApprovalAllowTools}
}

func (openCodeApproval) ModeHelp(policy string) string {
	switch strings.TrimSpace(policy) {
	case OpenCodeApprovalDeny:
		return "Fail-closed approval default under access control: path-scoped reads and the tool baseline run, while edits, web, and unlisted tools are denied without prompting. Sandbox off ignores this setting."
	case OpenCodeApprovalAsk:
		return "Under access control, keep tools enabled and ask a human before representable edits and permitted web tools. ⚠ Detached agents can block waiting. Sandbox off ignores this setting."
	case OpenCodeApprovalAllowTools:
		return "Under access control, automatically allow scoped edits and explicitly enabled web tools while keeping the tool baseline enabled. Sandbox off ignores this setting."
	default:
		return ""
	}
}

func (openCodeApproval) ValidatePolicy(policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	switch policy {
	case "", OpenCodeApprovalDeny, OpenCodeApprovalAsk, OpenCodeApprovalAllowTools:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid opencode approval policy %q (want %s|%s|%s)",
			policy, OpenCodeApprovalDeny, OpenCodeApprovalAsk, OpenCodeApprovalAllowTools)
	}
}
