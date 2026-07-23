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
		return "In access-control mode, allow bash, glob, grep, lsp, task, and skill, matching tclaude's existing OpenCode behaviour. Sandbox off ignores this setting."
	case OpenCodeToolsAsk:
		return "In access-control mode, ask before bash, glob, grep, lsp, task, or skill runs. Sandbox off ignores this setting. ⚠ Detached agents can block waiting for a human response."
	case OpenCodeToolsDeny:
		return "In access-control mode, deny bash, glob, grep, lsp, task, and skill without prompting. Sandbox off ignores this setting."
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
