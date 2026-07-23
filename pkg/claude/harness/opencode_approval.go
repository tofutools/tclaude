package harness

import (
	"fmt"
	"strings"
)

const (
	// OpenCodeApprovalDeny is the unattended default: reads inside the
	// selected reach are allowed, while edits, shell, web, and unlisted tools
	// are denied rather than left waiting for a human.
	OpenCodeApprovalDeny = "deny"

	// OpenCodeApprovalAsk lets a human approve representable edits and web
	// tools. Shell remains denied under access-control because a command cannot
	// be honestly confined to the generated directory policy.
	OpenCodeApprovalAsk = "ask"

	// OpenCodeApprovalAllowTools automatically permits the audited built-ins
	// the sandbox/network policy can represent. It deliberately does not
	// auto-approve shell commands.
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
		return "Read-only, fail-closed default: edits, shell, web, and unlisted tools are denied without prompting."
	case OpenCodeApprovalAsk:
		return "Ask a human before representable edits and permitted web tools. ⚠ Detached agents can block waiting; shell is available only with sandbox off."
	case OpenCodeApprovalAllowTools:
		return "Automatically allow scoped edits and explicitly enabled web tools. Shell still requires a human with sandbox off and is disabled under access-control."
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
