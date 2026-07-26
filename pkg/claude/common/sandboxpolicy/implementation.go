package sandboxpolicy

import (
	"fmt"
	"strings"
)

// Implementation selects which layer owns OS-level sandbox enforcement for a
// harness launch.
type Implementation string

const (
	ImplementationHarnessBuiltin Implementation = "harness-builtin"
	ImplementationTclaudeLayer   Implementation = "tclaude-layer"
)

// NormalizeImplementation validates a launch's sandbox implementation.
// Empty retains the legacy harness-owned behavior.
func NormalizeImplementation(value string) (Implementation, error) {
	implementation := Implementation(strings.TrimSpace(value))
	if implementation == "" {
		return ImplementationHarnessBuiltin, nil
	}
	switch implementation {
	case ImplementationHarnessBuiltin, ImplementationTclaudeLayer:
		return implementation, nil
	default:
		return "", fmt.Errorf("invalid sandbox implementation %q (want %s or %s)",
			value, ImplementationHarnessBuiltin, ImplementationTclaudeLayer)
	}
}
