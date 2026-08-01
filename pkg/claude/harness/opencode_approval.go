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
	// tools. The tool-governance axis independently controls the built-in tool
	// baseline in every sandbox mode.
	OpenCodeApprovalAsk = "ask"

	// OpenCodeApprovalAllowTools automatically permits representable edits and
	// audited web tools. The tool-governance axis independently controls the
	// built-in tool baseline in every sandbox mode.
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
		return "Fail-closed approval default: path-scoped reads run, while edits, web, and unlisted permissions are denied without prompting. Built-in tools follow the separate Tool governance setting."
	case OpenCodeApprovalAsk:
		return "Ask a human before representable edits and permitted web tools. Built-in tools follow the separate Tool governance setting. ⚠ Detached agents can block waiting."
	case OpenCodeApprovalAllowTools:
		return "Automatically allow scoped edits and explicitly enabled web tools. Access-control tools remain governed by the separate Tool governance setting."
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
