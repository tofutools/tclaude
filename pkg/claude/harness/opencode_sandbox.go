package harness

import (
	"fmt"
	"strings"
)

// OpenCodeSandboxOff is OpenCode's only honest launch-containment posture.
// OpenCode permissions gate tools, but OpenCode does not provide an OS sandbox
// that can enforce tclaude filesystem or network policy.
const OpenCodeSandboxOff = "off"

// openCodeSandbox surfaces the absence of OS containment as an explicit,
// catalog-driven launch choice. Keeping the single mode in a real catalog
// makes the posture visible in spawn/profile UIs and persistable in launch
// profiles instead of silently presenting OpenCode as sandbox-capable.
type openCodeSandbox struct{}

func (openCodeSandbox) DefaultMode() string { return OpenCodeSandboxOff }

func (openCodeSandbox) Modes() []string { return []string{OpenCodeSandboxOff} }

func (openCodeSandbox) ModeHelp(mode string) string {
	if strings.TrimSpace(mode) != OpenCodeSandboxOff {
		return ""
	}
	return "⚠ No tclaude OS containment — OpenCode runs without tclaude filesystem or network sandboxing. OpenCode's own tool permission rules still apply."
}

func (openCodeSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", OpenCodeSandboxOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid opencode sandbox mode %q (want %s)", mode, OpenCodeSandboxOff)
	}
}
