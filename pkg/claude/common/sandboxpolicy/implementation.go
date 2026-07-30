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

// SupportsMountPaths reports whether an implementation on a platform can
// enforce a remapped filesystem grant (TCL-866).
//
// Projecting a host directory onto a different sandbox path requires a real
// mount namespace. Only tclaude's own outer layer on Linux has one:
//
//   - macOS Seatbelt is a path filter over the host namespace. It can allow or
//     deny a path but cannot make a directory appear anywhere else, which is
//     why the daemon-final bind path already refuses source≠target there.
//   - harness-builtin sandboxes receive path lists and confine the harness in
//     the host namespace; they have no place to express a projection at all.
//
// Callers must refuse rather than degrade. Mounting at the host path instead
// would break the authored contract in both directions: it exposes a path the
// operator did not authorize, and leaves the one they did authorize empty.
func SupportsMountPaths(implementation Implementation, goos string) bool {
	return implementation.UsesTclaudeLayer() && goos == "linux"
}

// ValidateMountPathSupport refuses a launch whose rules need a capability the
// resolved implementation does not have. The message names the missing
// capability and the exact rule, following the wording pattern the other
// unsupported_sandbox_profile_* refusals use.
func ValidateMountPathSupport(
	grants []FilesystemGrant,
	implementation Implementation,
	goos string,
) error {
	if SupportsMountPaths(implementation, goos) {
		return nil
	}
	for _, grant := range grants {
		if !grant.IsRemapped() {
			continue
		}
		return fmt.Errorf(
			"unsupported_sandbox_profile_mount_path: sandbox implementation %q on %s cannot mount %q at sandbox path %q; mounting a host directory at a different sandbox path requires a mount namespace, which only the Linux tclaude-layer provides",
			implementation, goos, grant.Path, grant.GuestPath())
	}
	return nil
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
