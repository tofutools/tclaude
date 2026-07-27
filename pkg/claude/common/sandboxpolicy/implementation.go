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
	ImplementationStacked        Implementation = "stacked"
)

// UsesTclaudeLayer reports whether tclaude owns an outer OS boundary.
func (implementation Implementation) UsesTclaudeLayer() bool {
	return implementation == ImplementationTclaudeLayer || implementation == ImplementationStacked
}

// UsesNestedHarnessSandbox reports whether the harness's native OS sandbox is
// intentionally active beneath the tclaude-owned outer boundary.
func (implementation Implementation) UsesNestedHarnessSandbox() bool {
	return implementation == ImplementationStacked
}

// NormalizeImplementation validates a launch's sandbox implementation.
// Empty retains the legacy harness-owned behavior.
func NormalizeImplementation(value string) (Implementation, error) {
	implementation := Implementation(strings.TrimSpace(value))
	if implementation == "" {
		return ImplementationHarnessBuiltin, nil
	}
	switch implementation {
	case ImplementationHarnessBuiltin, ImplementationTclaudeLayer, ImplementationStacked:
		return implementation, nil
	default:
		return "", fmt.Errorf("invalid sandbox implementation %q (want %s, %s, or %s)",
			value, ImplementationHarnessBuiltin, ImplementationTclaudeLayer, ImplementationStacked)
	}
}
