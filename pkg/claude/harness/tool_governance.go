package harness

import (
	"fmt"
	"strings"
)

// ToolGovernanceCatalog is the optional launch-time policy for a harness's
// built-in tool baseline. It is independent from ApprovalCatalog: approval
// controls representable edit/web/environment actions, while tool governance
// controls only the harness-defined homogeneous tool group.
type ToolGovernanceCatalog interface {
	DefaultPolicy() string
	ValidatePolicy(policy string) (string, error)
	Modes() []string
	ModeHelp(policy string) string
}

// ResolveToolGovernance applies the harness default at daemon-owned spawn
// boundaries. A blank OpenCode value therefore remains backward-compatible
// with the pre-axis behaviour, while unsupported harnesses still omit it.
func ResolveToolGovernance(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" && h.SupportsToolGovernance() {
		requested = h.ToolGovernance.DefaultPolicy()
	}
	return ValidateToolGovernance(h, requested)
}

// ValidateToolGovernance validates without applying a default. Direct
// `session new` and profile-authoring paths use this so blank remains blank;
// an explicit value for a harness without the axis is rejected.
func ValidateToolGovernance(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	if !h.SupportsToolGovernance() {
		return "", fmt.Errorf("harness %q has no launch-time tool-governance policy", h.Name)
	}
	return h.ToolGovernance.ValidatePolicy(requested)
}
