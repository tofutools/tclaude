package harness

import (
	"fmt"
	"strings"
)

const (
	OpenCodeToolsAllow = "allow"
	OpenCodeToolsAsk   = "ask"
	OpenCodeToolsDeny  = "deny"
)

type openCodeToolGovernance struct{}

func (openCodeToolGovernance) DefaultPolicy() string { return OpenCodeToolsAllow }

func (openCodeToolGovernance) Modes() []string {
	return []string{OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny}
}

func (openCodeToolGovernance) ModeHelp(policy string) string {
	switch strings.TrimSpace(policy) {
	case OpenCodeToolsAllow:
		return "Allow bash, glob, grep, lsp, and skill in every sandbox mode. OpenCode sub-agents (task) are always off under tclaude."
	case OpenCodeToolsAsk:
		return "Ask before bash, glob, grep, lsp, or skill runs in every sandbox mode. OpenCode sub-agents (task) are always off under tclaude. ⚠ Detached agents can block waiting for a human response."
	case OpenCodeToolsDeny:
		return "Deny bash, glob, grep, lsp, and skill without prompting in every sandbox mode. OpenCode sub-agents (task) are always off under tclaude."
	default:
		return ""
	}
}

func (openCodeToolGovernance) ValidatePolicy(policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	switch policy {
	case "", OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid opencode tool-governance policy %q (want %s|%s|%s)",
			policy, OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny)
	}
}
