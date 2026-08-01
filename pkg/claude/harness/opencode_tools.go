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
		return "Allow bash, glob, grep, lsp, task, and skill in every sandbox mode."
	case OpenCodeToolsAsk:
		return "Ask before bash, glob, grep, lsp, task, or skill runs in every sandbox mode. ⚠ Detached agents can block waiting for a human response."
	case OpenCodeToolsDeny:
		return "Deny bash, glob, grep, lsp, task, and skill without prompting in every sandbox mode."
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
