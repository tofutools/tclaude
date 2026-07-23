package harness

import (
	"fmt"
	"strings"
)

const (
	// OpenCodeSandboxAccessControl applies a tclaude-generated OpenCode
	// permission ruleset. It limits the built-in read/edit tools with validated
	// lexical path patterns, but it is not an OS sandbox: symlink traversal and
	// disk access through tools without path-scoped permission keys are not
	// contained.
	OpenCodeSandboxAccessControl = "access-control"

	// OpenCodeSandboxOff disables directory scoping. Approval policy still
	// applies, so selecting off does not erase the fail-closed tool posture.
	OpenCodeSandboxOff = "off"
)

// openCodeSandbox surfaces both tclaude's soft access-control policy and the
// explicit no-scoping posture. Keeping them in a real catalog makes the
// distinction visible in spawn/profile UIs and persistable in launch profiles
// without misrepresenting either one as an OS sandbox.
type openCodeSandbox struct{}

func (openCodeSandbox) DefaultMode() string { return OpenCodeSandboxAccessControl }

func (openCodeSandbox) Modes() []string {
	return []string{OpenCodeSandboxAccessControl, OpenCodeSandboxOff}
}

func (openCodeSandbox) ModeHelp(mode string) string {
	switch strings.TrimSpace(mode) {
	case OpenCodeSandboxAccessControl:
		return "Lexical soft disk access control: built-in reads/edits follow relative path rules, while tools remain enabled. This is not an OS sandbox: it does not resolve or contain symlink targets, and bash/glob/grep can reach disk outside those lexical path rules."
	case OpenCodeSandboxOff:
		return "⚠ No directory scoping or OS containment. Filesystem/network sandbox profiles are incompatible and fail the launch. The selected tool approval policy still applies; bash is never auto-approved."
	default:
		return ""
	}
}

func (openCodeSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", OpenCodeSandboxAccessControl, OpenCodeSandboxOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid opencode sandbox mode %q (want %s|%s)",
			mode, OpenCodeSandboxAccessControl, OpenCodeSandboxOff)
	}
}
